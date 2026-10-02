package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	// Registered with database/sql for the connections db_query/db_schema open.
	"github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	_ "modernc.org/sqlite"             // registers the "sqlite" database/sql driver
)

var (
	errDBNameMissing       = errors.New("database name not configured (config/database.php or DB_DATABASE)")
	errDBUnsupportedDriver = errors.New("unsupported database driver")
)

// sqliteDirPerm is the mode of a database directory openDB has to create:
// the usual project-directory mode, which the web server user may need to
// traverse.
const sqliteDirPerm = 0o755

// dbDefaultHost is where Laravel's stock config points when DB_HOST is unset.
const dbDefaultHost = "127.0.0.1"

// Connection settings keys in config/database.php.
const (
	dbKeyHost     = "host"
	dbKeyPort     = "port"
	dbKeyUsername = "username"
)

// appDB is an open handle on one of the app's databases. The caller owns it and
// must Close it.
type appDB struct {
	*sql.DB

	driver string // normalized: "mysql", "pgsql" or "sqlite"
	schema string // schema name used for introspection
}

// openDB opens a named database connection (or the default). Connection
// settings come from config/database.php (fully parsed, env-resolved); if that
// can't be read it falls back to the DB_* .env variables.
func (p *Project) openDB(ctx context.Context, connection string) (*appDB, error) {
	connCfg := p.resolveConnConfig(ctx, connection)
	driver := normalizeDriver(cfgStr(connCfg, keyDriver))

	var (
		sqlDB  *sql.DB
		schema string
		err    error
	)

	switch driver {
	case driverMySQL:
		sqlDB, schema, err = openMySQL(connCfg)
	case driverPgSQL:
		sqlDB, schema, err = openPgSQL(connCfg)
	case driverSQLite:
		sqlDB, schema, err = p.openSQLite(connCfg)
	default:
		return nil, fmt.Errorf(
			"%w %q (supported: mysql, mariadb, pgsql, sqlite)",
			errDBUnsupportedDriver,
			driver,
		)
	}

	if err != nil {
		return nil, err
	}

	return &appDB{DB: sqlDB, driver: driver, schema: schema}, nil
}

// openMySQL opens a MySQL/MariaDB connection; the schema is the database name.
func openMySQL(connCfg map[string]any) (*sql.DB, string, error) {
	name := cfgStr(connCfg, keyDatabase)
	if name == "" {
		return nil, "", errDBNameMissing
	}

	host := orDefault(cfgStr(connCfg, dbKeyHost), dbDefaultHost)
	port := orDefault(cfgStr(connCfg, dbKeyPort), "3306")
	// The driver's own Config does the escaping, so credentials with '@',
	// '/' or '?' survive intact.
	mysqlCfg := mysql.NewConfig()
	mysqlCfg.User = cfgStr(connCfg, dbKeyUsername)
	mysqlCfg.Passwd = cfgStr(connCfg, keyPassword)
	mysqlCfg.Net = "tcp"
	mysqlCfg.Addr = net.JoinHostPort(host, port)
	mysqlCfg.DBName = name
	mysqlCfg.ParseTime = true

	sqlDB, err := sql.Open(driverMySQL, mysqlCfg.FormatDSN())
	if err != nil {
		return nil, "", fmt.Errorf("open mysql connection: %w", err)
	}

	return sqlDB, name, nil
}

// openPgSQL opens a Postgres connection; the schema is the search_path.
func openPgSQL(connCfg map[string]any) (*sql.DB, string, error) {
	name := cfgStr(connCfg, keyDatabase)
	if name == "" {
		return nil, "", errDBNameMissing
	}

	host := orDefault(cfgStr(connCfg, dbKeyHost), dbDefaultHost)
	port := orDefault(cfgStr(connCfg, dbKeyPort), "5432")
	ssl := orDefault(cfgStr(connCfg, "sslmode"), "prefer")
	schema := orDefault(cfgStr(connCfg, "search_path"), "public")

	var dsn url.URL

	dsn.Scheme = "postgres"
	dsn.User = url.UserPassword(cfgStr(connCfg, dbKeyUsername), cfgStr(connCfg, keyPassword))
	dsn.Host = net.JoinHostPort(host, port)
	dsn.Path = "/" + name
	dsn.RawQuery = url.Values{"sslmode": {ssl}}.Encode()

	sqlDB, err := sql.Open("pgx", dsn.String())
	if err != nil {
		return nil, "", fmt.Errorf("open pgsql connection: %w", err)
	}

	return sqlDB, schema, nil
}

// openSQLite opens the app's SQLite file, creating its directory if needed.
func (p *Project) openSQLite(connCfg map[string]any) (*sql.DB, string, error) {
	path := cfgStr(connCfg, keyDatabase)
	if path == "" || path == ":memory:" {
		path = p.path(keyDatabase, "database.sqlite")
	} else if !filepath.IsAbs(path) {
		path = p.path(path)
	}
	// A fresh app has no database file yet. SQLite creates the file on
	// first use, as `php artisan migrate` offers to; make sure its
	// directory exists too so that works for a custom path.
	err := os.MkdirAll(filepath.Dir(path), sqliteDirPerm)
	if err != nil {
		return nil, "", fmt.Errorf("create sqlite database directory: %w", err)
	}

	sqlDB, err := sql.Open(driverSQLite, path)
	if err != nil {
		return nil, "", fmt.Errorf("open sqlite database: %w", err)
	}

	return sqlDB, "main", nil
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
	pv, found, err := p.configViaPHP(ctx, "database.connections."+conn)
	if err != nil || !found {
		return nil, false
	}

	m, ok := pv.(map[string]any)

	return m, ok
}

// openDBPinged opens a connection and verifies it is reachable. On any failure
// the (possibly opened) handle is closed and a descriptive error returned; on
// success the caller owns the handle and must Close it.
func (p *Project) openDBPinged(ctx context.Context, connection string) (*appDB, error) {
	handle, err := p.openDB(ctx, connection)
	if err != nil {
		return nil, err
	}

	err = handle.PingContext(ctx)
	if err != nil {
		_ = handle.Close()

		return nil, fmt.Errorf("could not connect to database: %w", err)
	}

	return handle, nil
}

// resolveConnConfig returns the settings for one connection, preferring
// config/database.php and falling back to a map synthesized from .env.
func (p *Project) resolveConnConfig(ctx context.Context, connection string) map[string]any {
	if cc, ok := p.connFromConfig(ctx, connection); ok {
		return cc
	}

	conn := connection
	if conn == "" {
		conn = p.Env("DB_CONNECTION", driverMySQL)
	}

	return map[string]any{
		keyDriver:     conn,
		dbKeyHost:     p.Env("DB_HOST", ""),
		dbKeyPort:     p.Env("DB_PORT", ""),
		keyDatabase:   p.Env("DB_DATABASE", ""),
		dbKeyUsername: p.Env("DB_USERNAME", ""),
		keyPassword:   p.Env("DB_PASSWORD", ""),
	}
}

func normalizeDriver(name string) string {
	switch name {
	case driverMySQL, "mariadb":
		return driverMySQL
	case driverPgSQL, "postgres", "postgresql":
		return driverPgSQL
	case driverSQLite, "sqlite3":
		return driverSQLite
	default:
		return name
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

// queryer is what queryRows reads through: a *sql.DB, *sql.Conn or *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// queryRows runs an arbitrary SQL query and returns a generic column/row result
// with NULLs as nil and []byte stringified.
func queryRows(ctx context.Context, db queryer, query string, args ...any) (*queryResult, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("run query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("read result columns: %w", err)
	}

	res := &queryResult{Columns: cols, Rows: nil, Count: 0}
	for rows.Next() {
		row, scanErr := scanRow(rows, len(cols))
		if scanErr != nil {
			return nil, scanErr
		}

		res.Rows = append(res.Rows, row)
	}

	res.Count = len(res.Rows)

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("read result rows: %w", err)
	}

	return res, nil
}

// scanRow reads the current row generically, stringifying []byte values.
func scanRow(rows *sql.Rows, width int) ([]any, error) {
	raw := make([]any, width)

	ptrs := make([]any, width)
	for i := range raw {
		ptrs[i] = &raw[i]
	}

	err := rows.Scan(ptrs...)
	if err != nil {
		return nil, fmt.Errorf("scan result row: %w", err)
	}

	for i, v := range raw {
		if b, ok := v.([]byte); ok {
			raw[i] = string(b)
		}
	}

	return raw, nil
}

// ── Read-only query guard (db_query security boundary) ───────────────────────

var (
	leadingComment = regexp.MustCompile(`(?s)^\s*(--[^\n]*\n|/\*.*?\*/|\s)+`)
	// forbiddenKeyword matches data-modifying keywords refused anywhere in a
	// db_query.
	forbiddenKeyword = regexp.MustCompile(`(?i)\b(` + strings.Join([]string{
		"insert", "update", "delete", "drop", "alter", "create", "truncate",
		"replace", "merge", "grant", "revoke", "attach", "detach", "call",
		"exec", "execute", "into", "lock", "copy", "vacuum", "reindex",
		"comment", "rename",
	}, "|") + `)\b`)
)

// isAllowedLeadingKeyword reports whether a statement kind is one db_query
// permits.
func isAllowedLeadingKeyword(word string) bool {
	switch word {
	case "select", "show", "explain", "describe", "desc", "with", "pragma":
		return true
	default:
		return false
	}
}

// isReadOnlyQuery enforces the db_query allowlist: a single statement that
// begins with a read-only keyword and contains no data-modifying keyword. This
// is the security boundary — mirror of Boost's DatabaseQuery blocklist.
func isReadOnlyQuery(q string) (bool, string) {
	stmt := strings.TrimSpace(q)
	if stmt == "" {
		return false, "empty query"
	}
	// Strip leading comments/whitespace.
	stmt = leadingComment.ReplaceAllString(stmt, "")
	stmt = strings.TrimSpace(stmt)
	// Disallow multiple statements (allow a single trailing semicolon).
	body := strings.TrimRight(stmt, "; \t\n\r")
	if strings.IndexByte(body, ';') >= 0 {
		return false, "multiple statements are not allowed"
	}

	fields := strings.Fields(body)
	if len(fields) == 0 {
		return false, "empty query"
	}

	lead := strings.ToLower(fields[0])
	if !isAllowedLeadingKeyword(lead) {
		return false, fmt.Sprintf("only read-only statements are allowed (got %q)", lead)
	}

	if forbiddenKeyword.MatchString(body) {
		return false, "query contains a data-modifying keyword"
	}

	if lead == "pragma" {
		return readOnlyPragma(body)
	}

	return true, ""
}

// SQLite pragmas db_query may run. Most others change connection or database
// settings, by assignment or (optimize, wal_checkpoint, …) just by running.

// isIntrospectionPragma reports pragmas that only read, whatever their
// argument (a table or index name).
func isIntrospectionPragma(name string) bool {
	switch name {
	case "table_info", "table_xinfo", "table_list",
		"index_list", "index_info", "index_xinfo",
		"foreign_key_list", "foreign_key_check", "database_list",
		"collation_list", "function_list", "module_list", "pragma_list",
		"compile_options", "integrity_check", "quick_check":
		return true
	default:
		return false
	}
}

// isSettingPragma reports pragmas that read a setting when bare but write it
// when given a value, as `= v` or `(v)`, so they are allowed only without one.
func isSettingPragma(name string) bool {
	switch name {
	case "page_count", "page_size", "freelist_count", "encoding",
		"user_version", "schema_version", "application_id",
		"journal_mode", "foreign_keys":
		return true
	default:
		return false
	}
}

var pragmaRe = regexp.MustCompile(`(?i)^pragma\s+(?:\w+\.)?(\w+)\s*(.*)$`)

// readOnlyPragma allows a PRAGMA only when it reads: an introspection pragma, or
// a setting pragma with no value.
func readOnlyPragma(body string) (bool, string) {
	m := pragmaRe.FindStringSubmatch(body)
	if m == nil {
		return false, "malformed PRAGMA"
	}

	name, rest := strings.ToLower(m[1]), strings.TrimSpace(m[2])
	if strings.HasPrefix(rest, "=") {
		return false, "PRAGMA assignments are not allowed"
	}

	if isIntrospectionPragma(name) {
		return true, ""
	}

	if isSettingPragma(name) {
		if rest != "" {
			return false, fmt.Sprintf("PRAGMA %s with a value changes the setting", name)
		}

		return true, ""
	}

	return false, fmt.Sprintf("PRAGMA %s is not an allowed read-only pragma", name)
}

// queryReadOnly runs a db_query statement inside a read-only transaction that is
// always rolled back, so the database itself backs up isReadOnlyQuery: MySQL and
// Postgres reject writes in a READ ONLY transaction, and SQLite (whose driver
// ignores TxOptions.ReadOnly) gets query_only set on the connection instead.
func queryReadOnly(ctx context.Context, db *sql.DB, driver, query string) (*queryResult, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire database connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if driver == driverSQLite {
		_, err = conn.ExecContext(ctx, "PRAGMA query_only = ON")
		if err != nil {
			return nil, fmt.Errorf("make sqlite connection read-only: %w", err)
		}
		// The connection goes back to the pool on Close; don't leave it
		// read-only for whoever takes it next.
		defer func() { _, _ = conn.ExecContext(context.WithoutCancel(ctx), "PRAGMA query_only = OFF") }()
	}

	readTx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelDefault, ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin read-only transaction: %w", err)
	}

	defer func() { _ = readTx.Rollback() }()

	return queryRows(ctx, readTx, query)
}

// ── Schema introspection ─────────────────────────────────────────────────────

// schemaQuery is one introspection statement and the arguments it binds.
type schemaQuery struct {
	text string
	args []any
}

// listTablesQuery is the statement listing the schema's tables for a driver.
func listTablesQuery(driver, schema string) schemaQuery {
	switch driver {
	case driverMySQL:
		return schemaQuery{
			text: "SELECT table_name FROM information_schema.tables WHERE table_schema = ? ORDER BY table_name",
			args: []any{schema},
		}
	case driverPgSQL:
		return schemaQuery{
			text: "SELECT table_name FROM information_schema.tables " +
				"WHERE table_schema = $1 AND table_type='BASE TABLE' ORDER BY table_name",
			args: []any{schema},
		}
	case driverSQLite:
		return schemaQuery{
			text: "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name",
			args: nil,
		}
	default:
		// openDB only hands out the drivers above.
		return schemaQuery{text: "", args: nil}
	}
}

// listTables returns table names for the connection's schema.
func listTables(ctx context.Context, sqlDB *sql.DB, driver, schema string) ([]string, error) {
	query := listTablesQuery(driver, schema)

	res, err := queryRows(ctx, sqlDB, query.text, query.args...)
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

// tableQueries are the statements describeTable runs for one table.
type tableQueries struct {
	columns, indexes, foreignKeys schemaQuery
}

func mysqlTableQueries(schema, table string) tableQueries {
	args := []any{schema, table}

	return tableQueries{
		columns: schemaQuery{
			text: "SELECT column_name, column_type, is_nullable, column_default, column_key, extra " +
				"FROM information_schema.columns WHERE table_schema=? AND table_name=? ORDER BY ordinal_position",
			args: args,
		},
		indexes: schemaQuery{
			text: "SELECT index_name, column_name, non_unique, seq_in_index " +
				"FROM information_schema.statistics WHERE table_schema=? AND table_name=? " +
				"ORDER BY index_name, seq_in_index",
			args: args,
		},
		foreignKeys: schemaQuery{
			text: "SELECT column_name, referenced_table_name, referenced_column_name " +
				"FROM information_schema.key_column_usage " +
				"WHERE table_schema=? AND table_name=? AND referenced_table_name IS NOT NULL",
			args: args,
		},
	}
}

func pgsqlTableQueries(schema, table string) tableQueries {
	args := []any{schema, table}

	return tableQueries{
		columns: schemaQuery{
			text: "SELECT column_name, data_type, is_nullable, column_default " +
				"FROM information_schema.columns WHERE table_schema=$1 AND table_name=$2 ORDER BY ordinal_position",
			args: args,
		},
		indexes: schemaQuery{
			text: "SELECT indexname, indexdef FROM pg_indexes " +
				"WHERE schemaname=$1 AND tablename=$2 ORDER BY indexname",
			args: args,
		},
		foreignKeys: schemaQuery{
			text: `SELECT kcu.column_name, ccu.table_name AS referenced_table, ccu.column_name AS referenced_column
FROM information_schema.table_constraints tc
JOIN information_schema.key_column_usage kcu
  ON tc.constraint_name=kcu.constraint_name AND tc.table_schema=kcu.table_schema
JOIN information_schema.constraint_column_usage ccu
  ON ccu.constraint_name=tc.constraint_name AND ccu.table_schema=tc.table_schema
WHERE tc.constraint_type='FOREIGN KEY' AND tc.table_schema=$1 AND tc.table_name=$2`,
			args: args,
		},
	}
}

func sqliteTableQueries(table string) tableQueries {
	quoted := quoteIdent(table)

	return tableQueries{
		columns:     schemaQuery{text: "PRAGMA table_info(" + quoted + ")", args: nil},
		indexes:     schemaQuery{text: "PRAGMA index_list(" + quoted + ")", args: nil},
		foreignKeys: schemaQuery{text: "PRAGMA foreign_key_list(" + quoted + ")", args: nil},
	}
}

func describeTableQueries(driver, schema, table string) tableQueries {
	switch driver {
	case driverMySQL:
		return mysqlTableQueries(schema, table)
	case driverPgSQL:
		return pgsqlTableQueries(schema, table)
	case driverSQLite:
		return sqliteTableQueries(table)
	default:
		// openDB only hands out the drivers above.
		empty := schemaQuery{text: "", args: nil}

		return tableQueries{columns: empty, indexes: empty, foreignKeys: empty}
	}
}

// describeTable returns columns, indexes, and foreign keys for one table.
func describeTable(ctx context.Context, sqlDB *sql.DB, driver, schema, table string) (*tableDetail, error) {
	queries := describeTableQueries(driver, schema, table)

	cols, err := queryRows(ctx, sqlDB, queries.columns.text, queries.columns.args...)
	if err != nil {
		return nil, err
	}

	return &tableDetail{
		Table:       table,
		Columns:     cols,
		Indexes:     queryRowsOrNil(ctx, sqlDB, queries.indexes),
		ForeignKeys: queryRowsOrNil(ctx, sqlDB, queries.foreignKeys),
	}, nil
}

// queryRowsOrNil runs a best-effort query, yielding nil when it fails.
func queryRowsOrNil(ctx context.Context, db queryer, query schemaQuery) *queryResult {
	res, err := queryRows(ctx, db, query.text, query.args...)
	if err != nil {
		return nil
	}

	return res
}

// quoteIdent quotes a SQL identifier the standard way (pgsql, sqlite).
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// quoteIdentFor quotes a SQL identifier for the driver: MySQL uses backticks
// unless ANSI_QUOTES is on, everything else the standard double quotes.
func quoteIdentFor(driver, s string) string {
	if driver == driverMySQL {
		return "`" + strings.ReplaceAll(s, "`", "``") + "`"
	}

	return quoteIdent(s)
}

// rebind converts `?` placeholders to the driver's parameter style. mysql and
// sqlite use `?`; pgsql uses `$1`, `$2`, ….
func rebind(driver, query string) string {
	if driver != driverPgSQL {
		return query
	}

	parts := strings.Split(query, "?")

	var out strings.Builder

	out.WriteString(parts[0])

	for idx, part := range parts[1:] {
		out.WriteByte('$')
		out.WriteString(strconv.Itoa(idx + 1))
		out.WriteString(part)
	}

	return out.String()
}
