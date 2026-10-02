package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxQueryRows caps rows returned by db_query to keep results readable.
const maxQueryRows = 500

// dbMaskedPassword stands in for a configured password in db_connections.
const dbMaskedPassword = "********"

var errDBQueryRequired = errors.New("query is required")

func dbConnections(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	proj, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}

	proj.envName = argString(args, "env")

	// Prefer the fully-parsed config/database.php (lists every connection).
	if out, ok := connectionsFromConfig(ctx, proj); ok {
		return jsonResult(ctx, out), nil
	}

	return jsonResult(ctx, connectionsFromEnv(proj)), nil
}

// connectionsFromConfig lists every connection in config/database.php, with
// passwords masked; false when the config can't be read.
func connectionsFromConfig(ctx context.Context, proj *Project) (map[string]any, bool) {
	dbcfg, found, err := proj.config(keyDatabase)
	if err != nil || !found {
		return nil, false
	}

	note := ""
	// If connections are defined via spreads/dynamic code, re-resolve the
	// full set via PHP so none are omitted.
	if proj.evalLossy {
		pv, phpFound, phpErr := proj.configViaPHP(ctx, keyDatabase)
		if phpErr == nil && phpFound {
			dbcfg = pv
		} else {
			note = "some dynamically-defined connections may be omitted; PHP fallback unavailable"
		}
	}

	cfgMap, ok := dbcfg.(map[string]any)
	if !ok {
		return nil, false
	}

	out := map[string]any{
		"default":     cfgMap["default"],
		"connections": maskConnections(asMap(cfgMap["connections"])),
	}
	if note != "" {
		out[keyNote] = note
	}

	return out, true
}

// connectionsFromEnv is the fallback: the active connection from .env.
func connectionsFromEnv(proj *Project) map[string]any {
	masked := ""
	if proj.Env("DB_PASSWORD", "") != "" {
		masked = dbMaskedPassword
	}

	conn := proj.Env("DB_CONNECTION", driverMySQL)

	return map[string]any{
		"default": conn,
		"connections": map[string]any{
			conn: map[string]any{
				keyDriver:     conn,
				dbKeyHost:     proj.Env("DB_HOST", ""),
				dbKeyPort:     proj.Env("DB_PORT", ""),
				keyDatabase:   proj.Env("DB_DATABASE", ""),
				dbKeyUsername: proj.Env("DB_USERNAME", ""),
				keyPassword:   masked,
			},
		},
		keyNote: "config/database.php could not be read; showing the active connection from .env.",
	}
}

// maskConnections redacts password fields in each connection map.
func maskConnections(conns map[string]any) map[string]any {
	out := make(map[string]any, len(conns))
	for name, v := range conns {
		settings, ok := v.(map[string]any)
		if !ok {
			out[name] = v

			continue
		}

		masked := make(map[string]any, len(settings))
		for k, val := range settings {
			if k == keyPassword && val != nil && fmt.Sprint(val) != "" {
				masked[k] = dbMaskedPassword
			} else {
				masked[k] = val
			}
		}

		out[name] = masked
	}

	return out
}

func dbSchema(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	proj, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}

	proj.envName = argString(args, "env")

	sqlDB, err := proj.openDBPinged(ctx, argString(args, keyConnection))
	if err != nil {
		return toolResult{}, err
	}

	driver, schema := sqlDB.driver, sqlDB.schema

	defer func() { _ = sqlDB.Close() }()

	prefix := cfgStr(proj.resolveConnConfig(ctx, argString(args, keyConnection)), keyPrefix)

	if table := argString(args, keyTable); table != "" {
		detail, describeErr := describeTableWithPrefix(ctx, sqlDB.DB, driver, schema, table, prefix)
		if describeErr != nil {
			return toolResult{}, describeErr
		}

		return jsonResult(ctx, detail), nil
	}

	tables, err := listTables(ctx, sqlDB.DB, driver, schema)
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

// describeTableWithPrefix describes table; for apps with a table prefix it
// retries with the prefix if the bare name matched nothing.
func describeTableWithPrefix(
	ctx context.Context,
	sqlDB *sql.DB,
	driver, schema, table, prefix string,
) (*tableDetail, error) {
	detail, err := describeTable(ctx, sqlDB, driver, schema, table)
	if err != nil {
		return nil, err
	}

	if prefix == "" || hasColumns(detail) || strings.HasPrefix(table, prefix) {
		return detail, nil
	}

	prefixed, err := describeTable(ctx, sqlDB, driver, schema, prefix+table)
	if err == nil && hasColumns(prefixed) {
		return prefixed, nil
	}

	return detail, nil
}

func hasColumns(detail *tableDetail) bool {
	return detail.Columns != nil && len(detail.Columns.Rows) > 0
}

func dbQuery(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	query := argString(args, "query")
	if query == "" {
		return toolResult{}, errDBQueryRequired
	}

	if ok, reason := isReadOnlyQuery(query); !ok {
		return toolErrResult("Refused: " + reason +
			". db_query only runs read-only statements " +
			"(SELECT/SHOW/EXPLAIN/DESCRIBE/WITH/PRAGMA)."), nil
	}

	proj, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}

	proj.envName = argString(args, "env")

	sqlDB, err := proj.openDBPinged(ctx, argString(args, keyConnection))
	if err != nil {
		return toolResult{}, err
	}

	driver := sqlDB.driver

	defer func() { _ = sqlDB.Close() }()

	res, err := queryReadOnly(ctx, sqlDB.DB, driver, query)
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
		out.Content = append(
			out.Content,
			contentBlock{Type: contentText, Text: fmt.Sprintf("(truncated to first %d rows)", maxQueryRows)},
		)
	}

	return out, nil
}

// toolErrResult is an isError result carrying a message (no Go error).
func toolErrResult(msg string) toolResult {
	return toolResult{Content: []contentBlock{{Type: contentText, Text: msg}}, IsError: true}
}
