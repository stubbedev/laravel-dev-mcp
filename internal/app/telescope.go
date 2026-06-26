package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// telescopeTypes maps the tool's friendly type names to Telescope's `type`
// column values.
var telescopeTypes = map[string]string{
	"requests":      "request",
	"queries":       "query",
	"exceptions":    "exception",
	"logs":          "log",
	"http_client":   "client_request",
	"mail":          "mail",
	"notifications": "notification",
	"jobs":          "job",
	"events":        "event",
	"models":        "model",
	"cache":         "cache",
	"redis":         "redis",
	"schedule":      "schedule",
	"views":         "view",
	"dumps":         "dump",
	"commands":      "command",
	"gates":         "gate",
	"batches":       "batch",
}

type telescopeEntry struct {
	UUID      string `json:"uuid"`
	BatchID   string `json:"batch_id,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	Content   any    `json:"content"`
}

func telescope(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	friendly := argString(args, "type")
	etype, ok := telescopeTypes[friendly]
	if !ok {
		return toolResult{}, fmt.Errorf("unknown telescope type %q", friendly)
	}
	p, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}

	db, driver, _, err := p.openDBPinged(ctx, p.telescopeConn(args))
	if err != nil {
		return toolResult{}, err
	}
	defer func() { _ = db.Close() }()

	if !telescopeAvailable(ctx, db) {
		return telescopeUnavailable(p), nil
	}

	if id := argString(args, "id"); id != "" {
		res, err := queryRows(
			ctx,
			db,
			rebind(driver, "SELECT uuid, batch_id, content, created_at FROM telescope_entries WHERE uuid = ?"),
			id,
		)
		if err != nil {
			return toolResult{}, err
		}
		if len(res.Rows) == 0 {
			return textResult("No Telescope entry with uuid " + id), nil
		}
		return jsonResult(ctx, rowToEntry(res.Rows[0])), nil
	}

	q, qargs := buildTelescopeQuery(etype, args)
	res, err := queryRows(ctx, db, rebind(driver, q), qargs...)
	if err != nil {
		return toolResult{}, err
	}

	slow := argBool(args, "slow")
	entries := make([]telescopeEntry, 0, len(res.Rows))
	for _, row := range res.Rows {
		e := rowToEntry(row)
		if slow && etype == "query" && !isSlowQuery(e.Content) {
			continue
		}
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		return textResult(fmt.Sprintf("No Telescope %s entries found.", friendly)), nil
	}
	return jsonResult(ctx, entries), nil
}

// buildTelescopeQuery assembles the list query + args for an entry type and the
// optional filters (request_id, since_hours, tag, limit).
func buildTelescopeQuery(etype string, args map[string]any) (string, []any) {
	q := "SELECT uuid, batch_id, content, created_at FROM telescope_entries WHERE type = ?"
	qargs := []any{etype}
	if rid := argString(args, "request_id"); rid != "" {
		q += " AND batch_id = ?"
		qargs = append(qargs, rid)
	}
	if hours := argInt(args, "since_hours"); hours > 0 {
		cutoff := time.Now().UTC().Add(-time.Duration(hours) * time.Hour).Format("2006-01-02 15:04:05")
		q += " AND created_at >= ?"
		qargs = append(qargs, cutoff)
	}
	if tag := argString(args, "tag"); tag != "" {
		q += " AND uuid IN (SELECT entry_uuid FROM telescope_entries_tags WHERE tag = ?)"
		qargs = append(qargs, tag)
	}
	q += " ORDER BY sequence DESC LIMIT ?"
	return q, append(qargs, argClampInt(args, "limit", 50, 100))
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
		msg += "laravel/telescope is installed — run `php artisan migrate` and ensure TELESCOPE_ENABLED is on and the storage driver is 'database'."
	} else {
		msg += "laravel/telescope is not installed. Install it (composer require laravel/telescope) to use this tool. The other tools (db_query, logs, etc.) work without it."
	}
	return textResult(msg)
}

func rowToEntry(row []any) telescopeEntry {
	e := telescopeEntry{
		UUID:      fmt.Sprint(row[0]),
		CreatedAt: fmt.Sprint(row[3]),
	}
	if row[1] != nil {
		e.BatchID = fmt.Sprint(row[1])
	}
	if s := fmt.Sprint(row[2]); s != "" {
		var v any
		if json.Unmarshal([]byte(s), &v) == nil {
			e.Content = v
		} else {
			e.Content = s
		}
	}
	return e
}

// isSlowQuery reports whether a decoded query entry content is slow (>100ms),
// using Telescope's own `slow` flag when present, else the `time` field.
func isSlowQuery(content any) bool {
	m, ok := content.(map[string]any)
	if !ok {
		return false
	}
	if b, ok := m["slow"].(bool); ok && b {
		return true
	}
	switch t := m["time"].(type) {
	case float64:
		return t > 100
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f > 100
	}
	return false
}

// telescopeConn resolves which DB connection holds Telescope's entries: the
// explicit arg, else config/telescope.php's storage connection (null = default).
func (p *Project) telescopeConn(args map[string]any) string {
	if conn := argString(args, "connection"); conn != "" {
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
	p, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}
	db, driver, _, err := p.openDBPinged(ctx, p.telescopeConn(args))
	if err != nil {
		return toolResult{}, err
	}
	defer func() { _ = db.Close() }()
	if !telescopeAvailable(ctx, db) {
		return telescopeUnavailable(p), nil
	}

	hours := argClampInt(args, "hours", 24, 0)
	cutoff := time.Now().UTC().Add(-time.Duration(hours) * time.Hour).Format("2006-01-02 15:04:05")
	// telescope_entries_tags has an ON DELETE CASCADE FK on entry_uuid, so
	// deleting the entries clears their tags too.
	res, err := db.ExecContext(ctx, rebind(driver, "DELETE FROM telescope_entries WHERE created_at < ?"), cutoff)
	if err != nil {
		return toolResult{}, err
	}
	n, _ := res.RowsAffected()
	return textResult(fmt.Sprintf("Pruned %d Telescope entries older than %d hours.", n, hours)), nil
}
