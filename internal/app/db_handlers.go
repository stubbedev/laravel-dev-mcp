package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxQueryRows caps rows returned by db_query to keep results readable.
const maxQueryRows = 500

func dbConnections(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	p, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}
	p.envName = argString(args, "env")

	// Prefer the fully-parsed config/database.php (lists every connection).
	if dbcfg, found, cerr := p.config("database"); cerr == nil && found {
		note := ""
		// If connections are defined via spreads/dynamic code, re-resolve the
		// full set via PHP so none are omitted.
		if p.evalLossy {
			if pv, pf, perr := p.configViaPHP(ctx, "database"); perr == nil && pf {
				dbcfg = pv
			} else {
				note = "some dynamically-defined connections may be omitted; PHP fallback unavailable"
			}
		}
		if m, ok := dbcfg.(map[string]any); ok {
			conns, _ := m["connections"].(map[string]any)
			out := map[string]any{
				"default":     m["default"],
				"connections": maskConnections(conns),
			}
			if note != "" {
				out["note"] = note
			}
			return jsonResult(ctx, out), nil
		}
	}

	// Fallback: the active connection from .env.
	masked := ""
	if p.Env("DB_PASSWORD", "") != "" {
		masked = "********"
	}
	return jsonResult(ctx, map[string]any{
		"default": p.Env("DB_CONNECTION", "mysql"),
		"connections": map[string]any{
			p.Env("DB_CONNECTION", "mysql"): map[string]any{
				"driver":   p.Env("DB_CONNECTION", "mysql"),
				"host":     p.Env("DB_HOST", ""),
				"port":     p.Env("DB_PORT", ""),
				"database": p.Env("DB_DATABASE", ""),
				"username": p.Env("DB_USERNAME", ""),
				"password": masked,
			},
		},
		"note": "config/database.php could not be read; showing the active connection from .env.",
	}), nil
}

// maskConnections redacts password fields in each connection map.
func maskConnections(conns map[string]any) map[string]any {
	out := make(map[string]any, len(conns))
	for name, v := range conns {
		m, ok := v.(map[string]any)
		if !ok {
			out[name] = v
			continue
		}
		cp := make(map[string]any, len(m))
		for k, val := range m {
			if k == "password" && val != nil && fmt.Sprint(val) != "" {
				cp[k] = "********"
			} else {
				cp[k] = val
			}
		}
		out[name] = cp
	}
	return out
}

func dbSchema(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	p, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}
	p.envName = argString(args, "env")
	db, driver, schema, err := p.openDBPinged(ctx, argString(args, "connection"))
	if err != nil {
		return toolResult{}, err
	}
	defer func() { _ = db.Close() }()

	prefix := cfgStr(p.resolveConnConfig(ctx, argString(args, "connection")), "prefix")

	if table := argString(args, "table"); table != "" {
		d, err := describeTable(ctx, db, driver, schema, table)
		if err != nil {
			return toolResult{}, err
		}
		// Apps with a table prefix: retry with the prefix if the bare name
		// matched nothing.
		if prefix != "" && (d.Columns == nil || len(d.Columns.Rows) == 0) && !strings.HasPrefix(table, prefix) {
			if pd, perr := describeTable(
				ctx,
				db,
				driver,
				schema,
				prefix+table,
			); perr == nil && pd.Columns != nil &&
				len(pd.Columns.Rows) > 0 {
				d = pd
			}
		}
		return jsonResult(ctx, d), nil
	}

	tables, err := listTables(ctx, db, driver, schema)
	if err != nil {
		return toolResult{}, err
	}
	return jsonResult(ctx, struct {
		Driver string   `json:"driver"`
		Schema string   `json:"schema"`
		Prefix string   `json:"table_prefix,omitempty"`
		Tables []string `json:"tables"`
		Count  int      `json:"count"`
	}{driver, schema, prefix, tables, len(tables)}), nil
}

func dbQuery(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	query := argString(args, "query")
	if query == "" {
		return toolResult{}, errors.New("query is required")
	}
	if ok, reason := isReadOnlyQuery(query); !ok {
		return toolErrResult("Refused: " + reason +
			". db_query only runs read-only statements " +
			"(SELECT/SHOW/EXPLAIN/DESCRIBE/WITH/PRAGMA)."), nil
	}
	p, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}
	p.envName = argString(args, "env")
	db, _, _, err := p.openDBPinged(ctx, argString(args, "connection"))
	if err != nil {
		return toolResult{}, err
	}
	defer func() { _ = db.Close() }()

	res, err := queryRows(ctx, db, query)
	if err != nil {
		return toolResult{}, err
	}
	truncated := false
	if len(res.Rows) > maxQueryRows {
		res.Rows = res.Rows[:maxQueryRows]
		res.Count = maxQueryRows
		truncated = true
	}
	out := jsonResult(ctx, res)
	if truncated {
		out.Content = append(out.Content, contentBlock{Type: "text", Text: fmt.Sprintf("(truncated to first %d rows)", maxQueryRows)})
	}
	return out, nil
}

// toolErrResult is an isError result carrying a message (no Go error).
func toolErrResult(msg string) toolResult {
	return toolResult{Content: []contentBlock{{Type: "text", Text: msg}}, IsError: true}
}
