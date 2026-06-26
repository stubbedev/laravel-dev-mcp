package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// profile.go reads per-request profiling that Laravel profilers persist to disk
// — Clockwork (storage/clockwork) and Debugbar (storage/debugbar) — and
// normalizes both into one shape: timing plus a query breakdown with N+1
// (duplicate-query) detection. Pure file reads, no app boot. Degrades cleanly
// when neither profiler is recording.

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

func profile(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	p, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}
	limit := argClampInt(args, "limit", 5, 50)
	source := strings.ToLower(argString(args, "source"))

	clockwork := p.path("storage", "clockwork")
	debugbar := p.path("storage", "debugbar")

	// Auto-detect: prefer file-based profilers (cheap), else fall back to
	// Telescope, which records the same data in the database when installed.
	if source == "" {
		switch {
		case hasProfileData(clockwork, clockworkKeep):
			source = "clockwork"
		case hasProfileData(debugbar, debugbarKeep):
			source = "debugbar"
		default:
			source = "telescope"
		}
	}

	var files []string
	var parse func([]byte) (profileReq, bool)
	switch source {
	case "clockwork":
		files = newestFiles(clockwork, clockworkKeep, limit)
		parse = parseClockwork
	case "debugbar":
		files = newestFiles(debugbar, debugbarKeep, limit)
		parse = parseDebugbar
	case "telescope":
		return profileTelescope(ctx, p, args, limit)
	default:
		return toolErrResult("Refused: unknown source " + source + " (use clockwork, debugbar, or telescope)."), nil
	}
	if len(files) == 0 {
		return textResult("No " + source + " recordings in storage/" + source + "."), nil
	}

	reqs := make([]profileReq, 0, len(files))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if pr, ok := parse(b); ok {
			reqs = append(reqs, pr)
		}
	}
	return jsonResult(ctx, struct {
		Source   string       `json:"source"`
		Requests []profileReq `json:"requests"`
	}{source, reqs}), nil
}

// profileTelescope builds the same per-request profile from Telescope's
// telescope_entries (no Clockwork/Debugbar needed): the `request` entry gives
// timing, and the `query` entries for that batch give the query breakdown + N+1.
func profileTelescope(ctx context.Context, p *Project, args map[string]any, limit int) (toolResult, error) {
	db, driver, _, err := p.openDB(ctx, p.telescopeConn(args))
	if err != nil {
		return toolResult{}, err
	}
	defer func() { _ = db.Close() }()
	if err := db.PingContext(ctx); err != nil {
		return toolResult{}, fmt.Errorf("could not connect to database: %w", err)
	}
	if !telescopeAvailable(ctx, db) {
		return telescopeUnavailable(p), nil
	}

	reqRows, err := queryRows(ctx, db, rebind(driver,
		"SELECT batch_id, content FROM telescope_entries WHERE type = 'request' ORDER BY sequence DESC LIMIT ?"), limit)
	if err != nil {
		return toolResult{}, err
	}
	if len(reqRows.Rows) == 0 {
		return textResult("No Telescope request entries recorded yet. Make a request, then retry."), nil
	}

	order := make([]string, 0, len(reqRows.Rows))
	byBatch := map[string]*profileReq{}
	for _, row := range reqRows.Rows {
		batch := fmt.Sprint(row[0])
		m := asMap(decodeMaybe(fmt.Sprint(row[1])))
		byBatch[batch] = &profileReq{
			Source: "telescope", ID: batch,
			Method: asStr(m["method"]), URI: asStr(m["uri"]),
			Status: m["response_status"], DurationMs: asFloat(m["duration"]),
		}
		order = append(order, batch)
	}

	// Pull every query for those batches in one go, then group per request.
	placeholders := strings.TrimRight(strings.Repeat("?,", len(order)), ",")
	qargs := make([]any, len(order))
	for i, b := range order {
		qargs[i] = b
	}
	qRows, err := queryRows(ctx, db, rebind(driver,
		"SELECT batch_id, content FROM telescope_entries WHERE type = 'query' AND batch_id IN ("+placeholders+")"), qargs...)
	if err != nil {
		return toolResult{}, err
	}
	raws := map[string][]rawQuery{}
	for _, row := range qRows.Rows {
		batch := fmt.Sprint(row[0])
		m := asMap(decodeMaybe(fmt.Sprint(row[1])))
		raws[batch] = append(raws[batch], rawQuery{asStr(m["sql"]), asFloat(m["time"])}) // telescope time is ms
	}

	reqs := make([]profileReq, 0, len(order))
	for _, b := range order {
		pr := *byBatch[b]
		pr.Queries = summarizeQueries(raws[b])
		reqs = append(reqs, pr)
	}
	return jsonResult(ctx, struct {
		Source   string       `json:"source"`
		Requests []profileReq `json:"requests"`
	}{"telescope", reqs}), nil
}

// ── source detection / file listing ──────────────────────────────────────────

func clockworkKeep(name string) bool { return name != "index" && !strings.HasPrefix(name, ".") }
func debugbarKeep(name string) bool  { return strings.HasSuffix(name, ".json") }

func hasProfileData(dir string, keep func(string) bool) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && keep(e.Name()) {
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
	type fe struct {
		path string
		mod  int64
	}
	var fs []fe
	for _, e := range entries {
		if e.IsDir() || !keep(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		fs = append(fs, fe{filepath.Join(dir, e.Name()), info.ModTime().UnixNano()})
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].mod > fs[j].mod })
	if len(fs) > limit {
		fs = fs[:limit]
	}
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.path
	}
	return out
}

// ── per-source parsing ───────────────────────────────────────────────────────

func parseClockwork(b []byte) (profileReq, bool) {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return profileReq{}, false
	}
	pr := profileReq{
		Source:     "clockwork",
		ID:         asStr(m["id"]),
		Method:     asStr(m["method"]),
		URI:        asStr(m["uri"]),
		Status:     m["responseStatus"],
		DurationMs: asFloat(m["responseDuration"]),
	}
	var raws []rawQuery
	for _, q := range asArr(m["databaseQueries"]) {
		qm := asMap(q)
		raws = append(raws, rawQuery{asStr(qm["query"]), asFloat(qm["duration"])}) // clockwork duration is ms
	}
	pr.Queries = summarizeQueries(raws)
	return pr, true
}

func parseDebugbar(b []byte) (profileReq, bool) {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return profileReq{}, false
	}
	meta := asMap(m["__meta"])
	pr := profileReq{
		Source: "debugbar",
		ID:     asStr(meta["id"]),
		Method: asStr(meta["method"]),
		URI:    asStr(meta["uri"]),
	}
	if t := asMap(m["time"]); t != nil {
		pr.DurationMs = asFloat(t["duration"]) * 1000 // debugbar time is seconds
	}
	var raws []rawQuery
	if qd := asMap(m["queries"]); qd != nil {
		for _, s := range asArr(qd["statements"]) {
			sm := asMap(s)
			raws = append(raws, rawQuery{asStr(sm["sql"]), asFloat(sm["duration"]) * 1000}) // seconds → ms
		}
	}
	pr.Queries = summarizeQueries(raws)
	return pr, true
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
	out := profileQueries{Count: len(raws)}

	type grp struct {
		sql   string
		count int
	}
	groups := map[string]*grp{}
	var order []string
	for _, q := range raws {
		out.TotalMs += q.ms
		key := normalizeSQL(q.sql)
		if g := groups[key]; g != nil {
			g.count++
		} else {
			groups[key] = &grp{sql: key, count: 1}
			order = append(order, key)
		}
	}

	for _, k := range order {
		if g := groups[k]; g.count >= 2 {
			out.NPlusOne = append(out.NPlusOne, dupQ{SQL: g.sql, Count: g.count})
		}
	}
	sort.SliceStable(out.NPlusOne, func(i, j int) bool { return out.NPlusOne[i].Count > out.NPlusOne[j].Count })

	slow := append([]rawQuery(nil), raws...)
	sort.SliceStable(slow, func(i, j int) bool { return slow[i].ms > slow[j].ms })
	for i := 0; i < len(slow) && i < 3; i++ {
		if slow[i].ms <= 0 {
			break
		}
		out.Slowest = append(out.Slowest, slowQ{SQL: slow[i].sql, Ms: slow[i].ms})
	}
	return out
}

// ── any-shaped JSON helpers ──────────────────────────────────────────────────

func asStr(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func asFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	}
	return 0
}

func asArr(v any) []any {
	a, _ := v.([]any)
	return a
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}
