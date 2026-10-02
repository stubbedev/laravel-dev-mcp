package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	telescopeDefaultLimit = 50
	telescopeMaxLimit     = 100
	// telescopeSlowQueryMs mirrors Telescope's default slow-query threshold.
	telescopeSlowQueryMs       = 100
	telescopePruneDefaultHours = 24
)

// Telescope `type` values (and friendly names) that other parts of the package
// spell the same way, named so the lookup below reads as one table.
const (
	telescopeFriendlyModels = "models"
	telescopeTypeCache      = "cache"
	telescopeTypeRedis      = "redis"
)

var errTelescopeUnknownType = errors.New("unknown telescope type")

// telescopeTypes maps the tool's friendly type names to Telescope's `type`
// column values.
func telescopeTypes() map[string]string {
	return map[string]string{
		"requests":              "request",
		"queries":               "query",
		"exceptions":            "exception",
		"logs":                  "log",
		"http_client":           "client_request",
		"mail":                  "mail",
		"notifications":         "notification",
		"jobs":                  "job",
		"events":                "event",
		telescopeFriendlyModels: "model",
		telescopeTypeCache:      telescopeTypeCache,
		telescopeTypeRedis:      telescopeTypeRedis,
		"schedule":              "schedule",
		"views":                 "view",
		"dumps":                 "dump",
		"commands":              "command",
		"gates":                 "gate",
		"batches":               "batch",
	}
}

type telescopeEntry struct {
	UUID      string `json:"uuid"`
	BatchID   string `json:"batch_id,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	Content   any    `json:"content"`
}

func telescope(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	friendly := argString(args, "type")

	etype, ok := telescopeTypes()[friendly]
	if !ok {
		return toolResult{}, fmt.Errorf("%w %q", errTelescopeUnknownType, friendly)
	}

	proj, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}

	dbConn, err := proj.openDBPinged(ctx, proj.telescopeConn(args))
	if err != nil {
		return toolResult{}, err
	}

	driver := dbConn.driver
	defer func() { _ = dbConn.Close() }()

	if !telescopeAvailable(ctx, dbConn.DB) {
		return telescopeUnavailable(proj), nil
	}

	entryID := argString(args, "id")
	if entryID != "" {
		return telescopeEntryByID(ctx, dbConn.DB, driver, entryID)
	}

	query, qargs := buildTelescopeQuery(etype, args, proj.appNow())

	res, err := queryRows(ctx, dbConn, rebind(driver, query), qargs...)
	if err != nil {
		return toolResult{}, err
	}

	entries := filterTelescopeEntries(res.Rows, telescopeEntryFilter(etype, args))
	if len(entries) == 0 {
		return textResult(fmt.Sprintf("No Telescope %s entries found.", friendly)), nil
	}

	return jsonResult(ctx, entries), nil
}

func telescopeEntryByID(ctx context.Context, dbConn *sql.DB, driver, entryID string) (toolResult, error) {
	res, err := queryRows(
		ctx,
		dbConn,
		rebind(driver, "SELECT uuid, batch_id, content, created_at FROM telescope_entries WHERE uuid = ?"),
		entryID,
	)
	if err != nil {
		return toolResult{}, err
	}

	if len(res.Rows) == 0 {
		return textResult("No Telescope entry with uuid " + entryID), nil
	}

	return jsonResult(ctx, rowToEntry(res.Rows[0])), nil
}

// telescopeEntryFilter picks which listed entries to keep: only slow ones for
// queries with slow=true, else all.
func telescopeEntryFilter(etype string, args map[string]any) func(telescopeEntry) bool {
	if etype == "query" && argBool(args, "slow") {
		return func(entry telescopeEntry) bool { return isSlowQuery(entry.Content) }
	}

	return func(telescopeEntry) bool { return true }
}

// filterTelescopeEntries decodes rows into entries, keeping those keep accepts.
func filterTelescopeEntries(rows [][]any, keep func(telescopeEntry) bool) []telescopeEntry {
	entries := make([]telescopeEntry, 0, len(rows))
	for _, row := range rows {
		entry := rowToEntry(row)
		if !keep(entry) {
			continue
		}

		entries = append(entries, entry)
	}

	return entries
}

// telescopeTimeLayout is how created_at is compared: Laravel's datetime format.
const telescopeTimeLayout = time.DateTime

// appNow is the current time in the app's configured timezone (app.timezone),
// which is the zone Laravel stamps created_at columns in.
func (p *Project) appNow() time.Time {
	loc, err := time.LoadLocation(p.confStr("app.timezone", "UTC"))
	if err != nil {
		return time.Now().UTC()
	}

	return time.Now().In(loc)
}

// buildTelescopeQuery assembles the list query + args for an entry type and the
// optional filters (request_id, since_hours, tag, limit). now anchors
// since_hours, in the zone created_at is stored in.
func buildTelescopeQuery(etype string, args map[string]any, now time.Time) (string, []any) {
	query := "SELECT uuid, batch_id, content, created_at FROM telescope_entries WHERE type = ?"
	qargs := []any{etype}

	if rid := argString(args, "request_id"); rid != "" {
		query += " AND batch_id = ?"

		qargs = append(qargs, rid)
	}

	if hours := argInt(args, "since_hours"); hours > 0 {
		cutoff := now.Add(-time.Duration(hours) * time.Hour).Format(telescopeTimeLayout)
		query += " AND created_at >= ?"

		qargs = append(qargs, cutoff)
	}

	if tag := argString(args, "tag"); tag != "" {
		query += " AND uuid IN (SELECT entry_uuid FROM telescope_entries_tags WHERE tag = ?)"

		qargs = append(qargs, tag)
	}

	query += " ORDER BY sequence DESC LIMIT ?"

	return query, append(qargs, argClampInt(args, "limit", telescopeDefaultLimit, telescopeMaxLimit))
}

// telescopeAvailable reports whether the telescope_entries table is queryable.
func telescopeAvailable(ctx context.Context, db *sql.DB) bool {
	var x int

	err := db.QueryRowContext(ctx, "SELECT 1 FROM telescope_entries LIMIT 1").Scan(&x)

	return err == nil || errors.Is(err, sql.ErrNoRows)
}

func telescopeUnavailable(p *Project) toolResult {
	msg := "Telescope is not available: the telescope_entries table was not found. "
	if p.hasPackage("laravel/telescope") {
		msg += "laravel/telescope is installed — run `php artisan migrate` and ensure TELESCOPE_ENABLED is on " +
			"and the storage driver is 'database'."
	} else {
		msg += "laravel/telescope is not installed. Install it (composer require laravel/telescope) to use this " +
			"tool. The other tools (db_query, logs, etc.) work without it."
	}

	return textResult(msg)
}

func rowToEntry(row []any) telescopeEntry {
	batchID := ""
	if row[1] != nil {
		batchID = fmt.Sprint(row[1])
	}

	return telescopeEntry{
		UUID:      fmt.Sprint(row[0]),
		BatchID:   batchID,
		CreatedAt: fmt.Sprint(row[3]),
		Content:   decodeTelescopeContent(fmt.Sprint(row[2])),
	}
}

// decodeTelescopeContent returns the entry's JSON content decoded, or the raw
// string when it isn't valid JSON (nil when empty).
func decodeTelescopeContent(raw string) any {
	if raw == "" {
		return nil
	}

	var decoded any
	if json.Unmarshal([]byte(raw), &decoded) == nil {
		return decoded
	}

	return raw
}

// isSlowQuery reports whether a decoded query entry content is slow (>100ms),
// using Telescope's own `slow` flag when present, else the `time` field.
func isSlowQuery(content any) bool {
	entry, ok := content.(map[string]any)
	if !ok {
		return false
	}

	if slow, ok := entry["slow"].(bool); ok && slow {
		return true
	}

	return asFloat(entry["time"]) > telescopeSlowQueryMs
}

// telescopeConn resolves which DB connection holds Telescope's entries: the
// explicit arg, else config/telescope.php's storage connection (null = default).
func (p *Project) telescopeConn(args map[string]any) string {
	if conn := argString(args, keyConnection); conn != "" {
		return conn
	}

	if v, found, _ := p.config("telescope.storage.database.connection"); found {
		if s, ok := v.(string); ok {
			return s
		}
	}

	return ""
}

func telescopePrune(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	proj, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}

	dbConn, err := proj.openDBPinged(ctx, proj.telescopeConn(args))
	if err != nil {
		return toolResult{}, err
	}

	driver := dbConn.driver

	defer func() { _ = dbConn.Close() }()

	if !telescopeAvailable(ctx, dbConn.DB) {
		return telescopeUnavailable(proj), nil
	}

	hours := argClampInt(args, "hours", telescopePruneDefaultHours, 0)
	cutoff := proj.appNow().Add(-time.Duration(hours) * time.Hour).Format(telescopeTimeLayout)
	// telescope_entries_tags has an ON DELETE CASCADE FK on entry_uuid, so
	// deleting the entries clears their tags too.
	res, err := dbConn.ExecContext(ctx, rebind(driver, "DELETE FROM telescope_entries WHERE created_at < ?"), cutoff)
	if err != nil {
		return toolResult{}, fmt.Errorf("pruning telescope entries: %w", err)
	}

	pruned, _ := res.RowsAffected()

	return textResult(fmt.Sprintf("Pruned %d Telescope entries older than %d hours.", pruned, hours)), nil
}
