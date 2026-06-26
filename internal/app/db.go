package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	// Registered with database/sql for the connections db_query/db_schema open.
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

// openDB opens a named database connection (or the default). Connection
// settings come from config/database.php (fully parsed, env-resolved); if that
// can't be read it falls back to the DB_* .env variables. Returns the *sql.DB,
// the normalized driver ("mysql", "pgsql", "sqlite"), and the schema name used
// for introspection.
func (p *Project) openDB(ctx context.Context, connection string) (*sql.DB, string, string, error) {
	cc := p.resolveConnConfig(ctx, connection)
	driver := normalizeDriver(cfgStr(cc, "driver"))

	switch driver {
	case "mysql":
		name := cfgStr(cc, "database")
		if name == "" {
			return nil, "", "", errors.New("database name not configured (config/database.php or DB_DATABASE)")
		}
		host := orDefault(cfgStr(cc, "host"), "127.0.0.1")
		port := orDefault(cfgStr(cc, "port"), "3306")
		dsn := fmt.Sprintf("%s:%s@tcp(%s)/%s?parseTime=true",
			cfgStr(cc, "username"), cfgStr(cc, "password"), net.JoinHostPort(host, port), name)
		db, err := sql.Open("mysql", dsn)
		return db, "mysql", name, err

	case "pgsql":
		name := cfgStr(cc, "database")
		if name == "" {
			return nil, "", "", errors.New("database name not configured (config/database.php or DB_DATABASE)")
		}
		host := orDefault(cfgStr(cc, "host"), "127.0.0.1")
		port := orDefault(cfgStr(cc, "port"), "5432")
		ssl := orDefault(cfgStr(cc, "sslmode"), "prefer")
		schema := orDefault(cfgStr(cc, "search_path"), "public")
		dsn := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=%s",
			url.QueryEscape(cfgStr(cc, "username")), url.QueryEscape(cfgStr(cc, "password")),
			net.JoinHostPort(host, port), name, ssl)
		db, err := sql.Open("pgx", dsn)
		return db, "pgsql", schema, err

	case "sqlite":
		path := cfgStr(cc, "database")
		if path == "" || path == ":memory:" {
			path = p.path("database", "database.sqlite")
		} else if !filepath.IsAbs(path) {
			path = p.path(path)
		}
		db, err := sql.Open("sqlite", path)
		return db, "sqlite", "main", err

	default:
		return nil, "", "", fmt.Errorf("unsupported database driver %q (supported: mysql, mariadb, pgsql, sqlite)", driver)
	}
}

// connFromConfig looks up one connection's settings in config/database.php,
// falling back to a PHP eval for connections defined via spreads/dynamic code.
func (p *Project) connFromConfig(ctx context.Context, connection string) (map[string]any, bool) {
	conn := connection
	if conn == "" {
		if d, found, _ := p.config("database.default"); found {
			conn = fmt.Sprint(d)
		}
	}
	if conn == "" {
		return nil, false
	}
	// Statically-defined connection (the common case — fast, no PHP).
	if cc, found, _ := p.config("database.connections." + conn); found {
		if m, ok := cc.(map[string]any); ok {
			return m, true
		}
	}
	// Not found statically (e.g. a spread-generated connection) → ask PHP.
	if pv, found, err := p.configViaPHP(ctx, "database.connections."+conn); err == nil && found {
		if m, ok := pv.(map[string]any); ok {
			return m, true
		}
	}
	return nil, false
}

// openDBPinged opens a connection and verifies it is reachable. On any failure
// the (possibly opened) handle is closed and a descriptive error returned; on
// success the caller owns the handle and must Close it.
func (p *Project) openDBPinged(ctx context.Context, connection string) (*sql.DB, string, string, error) {
	db, driver, schema, err := p.openDB(ctx, connection)
	if err != nil {
		return nil, "", "", err
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, "", "", fmt.Errorf("could not connect to database: %w", err)
	}
	return db, driver, schema, nil
}

// resolveConnConfig returns the settings for one connection, preferring
// config/database.php and falling back to a map synthesized from .env.
func (p *Project) resolveConnConfig(ctx context.Context, connection string) map[string]any {
	if cc, ok := p.connFromConfig(ctx, connection); ok {
		return cc
	}
	conn := connection
	if conn == "" {
		conn = p.Env("DB_CONNECTION", "mysql")
	}
	return map[string]any{
		"driver":   conn,
		"host":     p.Env("DB_HOST", ""),
		"port":     p.Env("DB_PORT", ""),
		"database": p.Env("DB_DATABASE", ""),
		"username": p.Env("DB_USERNAME", ""),
		"password": p.Env("DB_PASSWORD", ""),
	}
}

func normalizeDriver(d string) string {
	switch d {
	case "mysql", "mariadb":
		return "mysql"
	case "pgsql", "postgres", "postgresql":
		return "pgsql"
	case "sqlite", "sqlite3":
		return "sqlite"
	default:
		return d
	}
}

// cfgStr reads a string-ish value from a config map (numbers stringified).
func cfgStr(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// ── Generic query → columns + rows ───────────────────────────────────────────

type queryResult struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
	Count   int      `json:"count"`
}

// queryRows runs an arbitrary SQL query and returns a generic column/row result
// with NULLs as nil and []byte stringified.
func queryRows(ctx context.Context, db *sql.DB, query string, args ...any) (*queryResult, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	res := &queryResult{Columns: cols}
	for rows.Next() {
		raw := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make([]any, len(cols))
		for i, v := range raw {
			if b, ok := v.([]byte); ok {
				row[i] = string(b)
			} else {
				row[i] = v
			}
		}
		res.Rows = append(res.Rows, row)
	}
	res.Count = len(res.Rows)
	return res, rows.Err()
}

// ── Read-only query guard (db_query security boundary) ───────────────────────

// forbiddenWords are data-modifying keywords refused anywhere in a db_query.
var forbiddenWords = []string{
	"insert", "update", "delete", "drop", "alter", "create", "truncate",
	"replace", "merge", "grant", "revoke", "attach", "detach", "call",
	"exec", "execute", "into", "lock", "copy", "vacuum", "reindex",
	"comment", "rename",
}

var (
	leadingComment   = regexp.MustCompile(`(?s)^\s*(--[^\n]*\n|/\*.*?\*/|\s)+`)
	forbiddenKeyword = regexp.MustCompile(`(?i)\b(` + strings.Join(forbiddenWords, "|") + `)\b`)
)

// allowedLeading are the only statement kinds db_query permits.
var allowedLeading = map[string]bool{
	"select": true, "show": true, "explain": true,
	"describe": true, "desc": true, "with": true, "pragma": true,
}

// isReadOnlyQuery enforces the db_query allowlist: a single statement that
// begins with a read-only keyword and contains no data-modifying keyword. This
// is the security boundary — mirror of Boost's DatabaseQuery blocklist.
func isReadOnlyQuery(q string) (bool, string) {
	s := strings.TrimSpace(q)
	if s == "" {
		return false, "empty query"
	}
	// Strip leading comments/whitespace.
	s = leadingComment.ReplaceAllString(s, "")
	s = strings.TrimSpace(s)
	// Disallow multiple statements (allow a single trailing semicolon).
	if i := strings.IndexByte(strings.TrimRight(s, "; \t\n\r"), ';'); i >= 0 {
		return false, "multiple statements are not allowed"
	}
	body := strings.TrimRight(s, "; \t\n\r")
	fields := strings.Fields(body)
	if len(fields) == 0 {
		return false, "empty query"
	}
	lead := strings.ToLower(fields[0])
	if !allowedLeading[lead] {
		return false, fmt.Sprintf("only read-only statements are allowed (got %q)", lead)
	}
	if forbiddenKeyword.MatchString(body) {
		return false, "query contains a data-modifying keyword"
	}
	return true, ""
}

// ── Schema introspection ─────────────────────────────────────────────────────

// listTables returns table names for the connection's schema.
func listTables(ctx context.Context, db *sql.DB, driver, schema string) ([]string, error) {
	var q string
	var args []any
	switch driver {
	case "mysql":
		q, args = "SELECT table_name FROM information_schema.tables WHERE table_schema = ? ORDER BY table_name", []any{schema}
	case "pgsql":
		q, args = "SELECT table_name FROM information_schema.tables WHERE table_schema = $1 AND table_type='BASE TABLE' ORDER BY table_name", []any{
			schema,
		}
	case "sqlite":
		q = "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name"
	}
	res, err := queryRows(ctx, db, q, args...)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(res.Rows))
	for _, r := range res.Rows {
		out = append(out, fmt.Sprint(r[0]))
	}
	return out, nil
}

type tableDetail struct {
	Table       string       `json:"table"`
	Columns     *queryResult `json:"columns"`
	Indexes     *queryResult `json:"indexes,omitempty"`
	ForeignKeys *queryResult `json:"foreign_keys,omitempty"`
}

// describeTable returns columns, indexes, and foreign keys for one table.
func describeTable(ctx context.Context, db *sql.DB, driver, schema, table string) (*tableDetail, error) {
	d := &tableDetail{Table: table}
	var colQ, idxQ, fkQ string
	var colArgs, idxArgs, fkArgs []any

	switch driver {
	case "mysql":
		colQ = "SELECT column_name, column_type, is_nullable, column_default, column_key, extra FROM information_schema.columns WHERE table_schema=? AND table_name=? ORDER BY ordinal_position"
		colArgs = []any{schema, table}
		idxQ = "SELECT index_name, column_name, non_unique, seq_in_index FROM information_schema.statistics WHERE table_schema=? AND table_name=? ORDER BY index_name, seq_in_index"
		idxArgs = []any{schema, table}
		fkQ = "SELECT column_name, referenced_table_name, referenced_column_name FROM information_schema.key_column_usage WHERE table_schema=? AND table_name=? AND referenced_table_name IS NOT NULL"
		fkArgs = []any{schema, table}
	case "pgsql":
		colQ = "SELECT column_name, data_type, is_nullable, column_default FROM information_schema.columns WHERE table_schema=$1 AND table_name=$2 ORDER BY ordinal_position"
		colArgs = []any{schema, table}
		idxQ = "SELECT indexname, indexdef FROM pg_indexes WHERE schemaname=$1 AND tablename=$2 ORDER BY indexname"
		idxArgs = []any{schema, table}
		fkQ = `SELECT kcu.column_name, ccu.table_name AS referenced_table, ccu.column_name AS referenced_column
FROM information_schema.table_constraints tc
JOIN information_schema.key_column_usage kcu ON tc.constraint_name=kcu.constraint_name AND tc.table_schema=kcu.table_schema
JOIN information_schema.constraint_column_usage ccu ON ccu.constraint_name=tc.constraint_name AND ccu.table_schema=tc.table_schema
WHERE tc.constraint_type='FOREIGN KEY' AND tc.table_schema=$1 AND tc.table_name=$2`
		fkArgs = []any{schema, table}
	case "sqlite":
		colQ = fmt.Sprintf("PRAGMA table_info(%s)", quoteIdent(table))
		idxQ = fmt.Sprintf("PRAGMA index_list(%s)", quoteIdent(table))
		fkQ = fmt.Sprintf("PRAGMA foreign_key_list(%s)", quoteIdent(table))
	}

	var err error
	if d.Columns, err = queryRows(ctx, db, colQ, colArgs...); err != nil {
		return nil, err
	}
	if d.Indexes, err = queryRows(ctx, db, idxQ, idxArgs...); err != nil {
		d.Indexes = nil // best-effort
	}
	if d.ForeignKeys, err = queryRows(ctx, db, fkQ, fkArgs...); err != nil {
		d.ForeignKeys = nil // best-effort
	}
	return d, nil
}

// quoteIdent quotes a SQL identifier for PRAGMA usage (sqlite).
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// rebind converts `?` placeholders to the driver's parameter style. mysql and
// sqlite use `?`; pgsql uses `$1`, `$2`, …
func rebind(driver, query string) string {
	if driver != "pgsql" {
		return query
	}
	var b strings.Builder
	n := 0
	for i := range len(query) {
		if query[i] == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(query[i])
	}
	return b.String()
}
