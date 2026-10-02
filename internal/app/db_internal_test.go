package app

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// slowQueryTimeKey is the query entry field isSlowQuery reads the duration from.
const slowQueryTimeKey = "time"

func TestIsReadOnlyQuery(t *testing.T) {
	t.Parallel()

	allowed := []string{
		"SELECT * FROM users",
		"  select id from posts where id = 1",
		"SHOW TABLES",
		"EXPLAIN SELECT * FROM users",
		"DESCRIBE users",
		"WITH t AS (SELECT 1) SELECT * FROM t",
		"PRAGMA table_info(users)",
		"PRAGMA main.index_list(users)",
		"PRAGMA journal_mode",
		"-- a comment\nSELECT 1",
		"/* block */ SELECT 1",
		"SELECT * FROM users;",
	}
	for _, q := range allowed {
		if good, reason := isReadOnlyQuery(q); !good {
			t.Errorf("expected allowed: %q (got %q)", q, reason)
		}
	}

	bad := []string{
		"",
		"DELETE FROM users",
		"UPDATE users SET name='x'",
		"INSERT INTO users VALUES (1)",
		"DROP TABLE users",
		"TRUNCATE users",
		"ALTER TABLE users ADD c INT",
		"SELECT 1; DROP TABLE users",
		"SELECT 1; SELECT 2",
		"SELECT * INTO OUTFILE '/tmp/x' FROM users",
		"WITH t AS (DELETE FROM users RETURNING *) SELECT * FROM t",
		"CREATE TABLE x (id int)",
		"PRAGMA journal_mode = DELETE",
		"PRAGMA journal_mode(WAL)",
		"PRAGMA user_version = 5",
		"PRAGMA query_only = 0",
		"PRAGMA optimize",
		"PRAGMA wal_checkpoint(TRUNCATE)",
	}
	for _, q := range bad {
		if good, _ := isReadOnlyQuery(q); good {
			t.Errorf("expected refused: %q", q)
		}
	}
}

func TestRebind(t *testing.T) {
	t.Parallel()

	got := rebind("pgsql", "SELECT id FROM t WHERE a = ? AND b = ?")

	want := "SELECT id FROM t WHERE a = $1 AND b = $2"
	if got != want {
		t.Errorf("rebind pgsql: got %q want %q", got, want)
	}

	if g := rebind("mysql", "a = ?"); g != "a = ?" {
		t.Errorf("rebind mysql should be unchanged, got %q", g)
	}
}

func TestIsSlowQuery(t *testing.T) {
	t.Parallel()

	cases := []struct {
		content any
		want    bool
	}{
		{map[string]any{"slow": true}, true},
		{map[string]any{slowQueryTimeKey: float64(150)}, true},
		{map[string]any{slowQueryTimeKey: float64(50)}, false},
		{map[string]any{slowQueryTimeKey: "120.5"}, true},
		{map[string]any{slowQueryTimeKey: "10"}, false},
		{map[string]any{}, false},
		{"not a map", false},
	}
	for i, c := range cases {
		if got := isSlowQuery(c.content); got != c.want {
			t.Errorf("case %d: got %v want %v", i, got, c.want)
		}
	}
}

// queryReadOnly must stop a write even if one got past isReadOnlyQuery, and
// must hand the pooled SQLite connection back writable.
func TestQueryReadOnlySQLite(t *testing.T) {
	t.Parallel()

	sqlDB, err := sql.Open(driverSQLite, filepath.Join(t.TempDir(), "ro.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlDB.Close() }()

	sqlDB.SetMaxOpenConns(1) // force the same connection to be reused

	ctx := t.Context()

	_, err = sqlDB.ExecContext(ctx, "CREATE TABLE t (id INTEGER)")
	if err != nil {
		t.Fatal(err)
	}

	_, err = queryReadOnly(ctx, sqlDB, driverSQLite, "INSERT INTO t VALUES (1) RETURNING id")
	if err == nil {
		t.Error("write through queryReadOnly succeeded, want an error")
	}

	res, err := queryReadOnly(ctx, sqlDB, driverSQLite, "SELECT count(*) FROM t")
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if got := res.Rows[0][0]; got != int64(0) {
		t.Errorf("row count = %v, want 0", got)
	}

	_, err = sqlDB.ExecContext(ctx, "INSERT INTO t VALUES (2)")
	if err != nil {
		t.Errorf("connection left read-only after queryReadOnly: %v", err)
	}
}

// A fresh app with the default sqlite connection has no database file yet;
// opening it creates one (directory included) instead of failing.
func TestOpenDBCreatesMissingSQLiteFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mkdir(t, filepath.Join(dir, "config"))
	write(t, filepath.Join(dir, "config", "database.php"), `<?php return [
    'default' => 'sqlite',
    'connections' => ['sqlite' => ['driver' => 'sqlite', 'database' => database_path('database.sqlite')]],
];`)

	p := newProject(dir)

	sqlDB, err := p.openDBPinged(t.Context(), "")
	if err != nil {
		t.Fatalf("openDBPinged: %v", err)
	}

	_ = sqlDB.Close()

	_, err = os.Stat(filepath.Join(dir, "database", "database.sqlite"))
	if err != nil {
		t.Errorf("database/database.sqlite not created: %v", err)
	}
}
