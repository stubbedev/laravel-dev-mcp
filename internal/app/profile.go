package app

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// profile.go reads per-request profiling that Laravel profilers persist to disk
// — Clockwork (storage/clockwork) and Debugbar (storage/debugbar) — and
// normalizes both into one shape: timing plus a query breakdown with N+1
// (duplicate-query) detection. Pure file reads, no app boot. Degrades cleanly
// when neither profiler is recording.

const (
	profileDefaultLimit = 5
	profileMaxLimit     = 50
	// profileNPlusOneMin is how many runs of one normalized statement make it
	// an N+1 suspect.
	profileNPlusOneMin = 2
	// profileSlowestKeep is how many of the slowest queries a request reports.
	profileSlowestKeep = 3
	// msPerSecond converts Debugbar's second-based timings to milliseconds.
	msPerSecond = 1000
)

const profileNoData = "No profiler data found. Install Clockwork or Debugbar (they write to storage/clockwork " +
	"or storage/debugbar), or install Telescope with a database connection. " +
	"Pass source=telescope to see the underlying error."

type profileReq struct {
	Source     string         `json:"source"`
	ID         string         `json:"id,omitempty"`
	Method     string         `json:"method,omitempty"`
	URI        string         `json:"uri,omitempty"`
	Status     any            `json:"status,omitempty"`
	DurationMs float64        `json:"duration_ms,omitempty"`
	Queries    profileQueries `json:"queries"`
}

type profileQueries struct {
	Count    int     `json:"count"`
	TotalMs  float64 `json:"total_ms"`
	NPlusOne []dupQ  `json:"n_plus_one,omitempty"` // queries that ran ≥2× (normalized)
	Slowest  []slowQ `json:"slowest,omitempty"`
}

type dupQ struct {
	SQL   string `json:"sql"`
	Count int    `json:"count"`
}

type slowQ struct {
	SQL string  `json:"sql"`
	Ms  float64 `json:"ms"`
}

type rawQuery struct {
	sql string
	ms  float64
}

// profileFileSource is a file-based profiler: where it writes, which entries
// in that directory are recordings, and how to parse one.
type profileFileSource struct {
	dir   string
	keep  func(string) bool
	parse func([]byte) (profileReq, bool)
}

func profile(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	proj, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}

	limit := argClampInt(args, "limit", profileDefaultLimit, profileMaxLimit)
	source := strings.ToLower(argString(args, "source"))

	autoDetected := source == ""
	if autoDetected {
		source = detectProfileSource(proj)
	}

	if source == sourceTelescope {
		res, err := profileTelescope(ctx, proj, args, limit)
		if err != nil && autoDetected {
			return textResult(profileNoData), nil
		}

		return res, err
	}

	fileSource, ok := profileFileSourceFor(proj, source)
	if !ok {
		return toolErrResult("Refused: unknown source " + source + " (use clockwork, debugbar, or telescope)."), nil
	}

	return profileFromFiles(ctx, source, fileSource, limit), nil
}

// detectProfileSource prefers file-based profilers (cheap), else falls back to
// Telescope, which records the same data in the database when installed.
func detectProfileSource(proj *Project) string {
	switch {
	case hasProfileData(proj.path("storage", sourceClockwork), clockworkKeep):
		return sourceClockwork
	case hasProfileData(proj.path("storage", sourceDebugbar), debugbarKeep):
		return sourceDebugbar
	default:
		return sourceTelescope
	}
}

func profileFileSourceFor(proj *Project, source string) (profileFileSource, bool) {
	switch source {
	case sourceClockwork:
		return profileFileSource{
			dir:   proj.path("storage", sourceClockwork),
			keep:  clockworkKeep,
			parse: parseClockwork,
		}, true
	case sourceDebugbar:
		return profileFileSource{
			dir:   proj.path("storage", sourceDebugbar),
			keep:  debugbarKeep,
			parse: parseDebugbar,
		}, true
	default:
		return profileFileSource{dir: "", keep: nil, parse: nil}, false
	}
}

func profileFromFiles(ctx context.Context, source string, fileSource profileFileSource, limit int) toolResult {
	files := newestFiles(fileSource.dir, fileSource.keep, limit)
	if len(files) == 0 {
		return textResult("No " + source + " recordings in storage/" + source + ".")
	}

	reqs := make([]profileReq, 0, len(files))
	for _, file := range files {
		// Recordings live in the project's own storage dir; reading them is the point.
		data, err := os.ReadFile(file) //nolint:gosec // G304: path comes from listing the profiler dir
		if err != nil {
			continue
		}

		if pr, ok := fileSource.parse(data); ok {
			reqs = append(reqs, pr)
		}
	}

	return profileResult(ctx, source, reqs)
}

func profileResult(ctx context.Context, source string, reqs []profileReq) toolResult {
	return jsonResult(ctx, struct {
		Source   string       `json:"source"`
		Requests []profileReq `json:"requests"`
	}{source, reqs})
}

// profileTelescope builds the same per-request profile from Telescope's
// telescope_entries (no Clockwork/Debugbar needed): the `request` entry gives
// timing, and the `query` entries for that batch give the query breakdown + N+1.
func profileTelescope(ctx context.Context, proj *Project, args map[string]any, limit int) (toolResult, error) {
	dbConn, err := proj.openDBPinged(ctx, proj.telescopeConn(args))
	if err != nil {
		return toolResult{}, err
	}

	driver := dbConn.driver
	defer func() { _ = dbConn.Close() }()

	if !telescopeAvailable(ctx, dbConn.DB) {
		return telescopeUnavailable(proj), nil
	}

	reqs, err := telescopeRequestProfiles(ctx, dbConn.DB, driver, limit)
	if err != nil {
		return toolResult{}, err
	}

	if len(reqs) == 0 {
		return textResult("No Telescope request entries recorded yet. Make a request, then retry."), nil
	}

	return profileResult(ctx, sourceTelescope, reqs), nil
}

// telescopeRequestProfiles reads the newest `request` entries and attaches each
// one's query breakdown. Empty when Telescope has recorded no requests yet.
func telescopeRequestProfiles(ctx context.Context, dbConn *sql.DB, driver string, limit int) ([]profileReq, error) {
	reqRows, err := queryRows(ctx, dbConn, rebind(driver,
		"SELECT batch_id, content FROM telescope_entries WHERE type = 'request' ORDER BY sequence DESC LIMIT ?"), limit)
	if err != nil {
		return nil, err
	}

	if len(reqRows.Rows) == 0 {
		return nil, nil
	}

	batches := make([]string, len(reqRows.Rows))
	for i, row := range reqRows.Rows {
		batches[i] = fmt.Sprint(row[0])
	}

	raws, err := telescopeBatchQueries(ctx, dbConn, driver, batches)
	if err != nil {
		return nil, err
	}

	reqs := make([]profileReq, 0, len(batches))
	for idx, row := range reqRows.Rows {
		batch := batches[idx]
		content := asMap(decodeMaybe(fmt.Sprint(row[1])))
		reqs = append(reqs, profileReq{
			Source:     sourceTelescope,
			ID:         batch,
			Method:     asStr(content["method"]),
			URI:        asStr(content["uri"]),
			Status:     content["response_status"],
			DurationMs: asFloat(content["duration"]),
			Queries:    summarizeQueries(raws[batch]),
		})
	}

	return reqs, nil
}

// telescopeBatchQueries pulls every query for the given batches in one go,
// grouped per batch (= per request).
func telescopeBatchQueries(
	ctx context.Context, dbConn *sql.DB, driver string, batches []string,
) (map[string][]rawQuery, error) {
	placeholders := strings.TrimRight(strings.Repeat("?,", len(batches)), ",")

	qargs := make([]any, len(batches))
	for i, batch := range batches {
		qargs[i] = batch
	}

	qRows, err := queryRows(ctx, dbConn, rebind(
		driver,
		"SELECT batch_id, content FROM telescope_entries WHERE type = 'query' AND batch_id IN ("+placeholders+")",
	), qargs...)
	if err != nil {
		return nil, err
	}

	raws := map[string][]rawQuery{}

	for _, row := range qRows.Rows {
		batch := fmt.Sprint(row[0])
		content := asMap(decodeMaybe(fmt.Sprint(row[1])))
		// telescope time is ms
		raws[batch] = append(raws[batch], rawQuery{asStr(content["sql"]), asFloat(content["time"])})
	}

	return raws, nil
}

// ── source detection / file listing ──────────────────────────────────────────

func clockworkKeep(name string) bool { return name != "index" && !strings.HasPrefix(name, ".") }
func debugbarKeep(name string) bool  { return strings.HasSuffix(name, ".json") }

func hasProfileData(dir string, keep func(string) bool) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}

	for _, entry := range entries {
		if !entry.IsDir() && keep(entry.Name()) {
			return true
		}
	}

	return false
}

func newestFiles(dir string, keep func(string) bool, limit int) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	type fileMod struct {
		path    string
		modNano int64
	}

	var files []fileMod

	for _, entry := range entries {
		if entry.IsDir() || !keep(entry.Name()) {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		files = append(files, fileMod{filepath.Join(dir, entry.Name()), info.ModTime().UnixNano()})
	}

	slices.SortFunc(files, func(a, b fileMod) int { return cmp.Compare(b.modNano, a.modNano) })

	if len(files) > limit {
		files = files[:limit]
	}

	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.path
	}

	return out
}

// ── per-source parsing ───────────────────────────────────────────────────────

func parseClockwork(data []byte) (profileReq, bool) {
	var doc map[string]any

	err := json.Unmarshal(data, &doc)
	if err != nil {
		var none profileReq

		return none, false
	}

	var raws []rawQuery

	for _, query := range asArr(doc["databaseQueries"]) {
		qm := asMap(query)
		raws = append(raws, rawQuery{asStr(qm["query"]), asFloat(qm["duration"])}) // clockwork duration is ms
	}

	return profileReq{
		Source:     sourceClockwork,
		ID:         asStr(doc["id"]),
		Method:     asStr(doc["method"]),
		URI:        asStr(doc["uri"]),
		Status:     doc["responseStatus"],
		DurationMs: asFloat(doc["responseDuration"]),
		Queries:    summarizeQueries(raws),
	}, true
}

func parseDebugbar(data []byte) (profileReq, bool) {
	var doc map[string]any

	err := json.Unmarshal(data, &doc)
	if err != nil {
		var none profileReq

		return none, false
	}

	meta := asMap(doc["__meta"])

	var raws []rawQuery

	for _, stmt := range asArr(asMap(doc["queries"])["statements"]) {
		sm := asMap(stmt)
		raws = append(raws, rawQuery{asStr(sm["sql"]), asFloat(sm["duration"]) * msPerSecond}) // seconds → ms
	}

	return profileReq{
		Source:     sourceDebugbar,
		ID:         asStr(meta["id"]),
		Method:     asStr(meta["method"]),
		URI:        asStr(meta["uri"]),
		Status:     nil,
		DurationMs: asFloat(asMap(doc["time"])["duration"]) * msPerSecond, // debugbar time is seconds
		Queries:    summarizeQueries(raws),
	}, true
}

// ── query analysis (shared) ──────────────────────────────────────────────────

var (
	reSQLStr = regexp.MustCompile(`'[^']*'`)
	reSQLNum = regexp.MustCompile(`\b\d+\b`)
	reSQLWS  = regexp.MustCompile(`\s+`)
)

// normalizeSQL collapses literals and whitespace so that the same statement run
// with different bound values groups together — the signal for an N+1.
func normalizeSQL(s string) string {
	s = reSQLStr.ReplaceAllString(s, "?")
	s = reSQLNum.ReplaceAllString(s, "?")

	return strings.TrimSpace(reSQLWS.ReplaceAllString(s, " "))
}

func summarizeQueries(raws []rawQuery) profileQueries {
	total := 0.0
	for _, query := range raws {
		total += query.ms
	}

	return profileQueries{
		Count:    len(raws),
		TotalMs:  total,
		NPlusOne: duplicateQueries(raws),
		Slowest:  slowestQueries(raws),
	}
}

// duplicateQueries groups statements by normalized SQL and returns those that
// ran often enough to be an N+1, most repeated first.
func duplicateQueries(raws []rawQuery) []dupQ {
	counts := map[string]int{}

	var order []string

	for _, query := range raws {
		key := normalizeSQL(query.sql)
		if counts[key] == 0 {
			order = append(order, key)
		}

		counts[key]++
	}

	var dups []dupQ

	for _, key := range order {
		if counts[key] >= profileNPlusOneMin {
			dups = append(dups, dupQ{SQL: key, Count: counts[key]})
		}
	}

	slices.SortStableFunc(dups, func(a, b dupQ) int { return cmp.Compare(b.Count, a.Count) })

	return dups
}

// slowestQueries returns the few slowest statements that took measurable time.
func slowestQueries(raws []rawQuery) []slowQ {
	slow := slices.Clone(raws)
	slices.SortStableFunc(slow, func(a, b rawQuery) int { return cmp.Compare(b.ms, a.ms) })

	var out []slowQ

	for _, query := range slow[:min(profileSlowestKeep, len(slow))] {
		if query.ms <= 0 {
			break
		}

		out = append(out, slowQ{SQL: query.sql, Ms: query.ms})
	}

	return out
}

// ── any-shaped JSON helpers ──────────────────────────────────────────────────

func asStr(value any) string {
	if value == nil {
		return ""
	}

	if s, ok := value.(string); ok {
		return s
	}

	return fmt.Sprint(value)
}

// asFloat reads a number that may arrive as a JSON number or, as Telescope
// writes query times (number_format), a numeric string.
func asFloat(value any) float64 {
	switch num := value.(type) {
	case float64:
		return num
	case int:
		return float64(num)
	case int64:
		return float64(num)
	case json.Number:
		f, _ := num.Float64()

		return f
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(num), 64)

		return f
	}

	return 0
}

func asArr(value any) []any {
	if arr, ok := value.([]any); ok {
		return arr
	}

	return nil
}

func asMap(value any) map[string]any {
	if obj, ok := value.(map[string]any); ok {
		return obj
	}

	return nil
}
