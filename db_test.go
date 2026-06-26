package main

import "testing"

func TestIsReadOnlyQuery(t *testing.T) {
	ok := []string{
		"SELECT * FROM users",
		"  select id from posts where id = 1",
		"SHOW TABLES",
		"EXPLAIN SELECT * FROM users",
		"DESCRIBE users",
		"WITH t AS (SELECT 1) SELECT * FROM t",
		"PRAGMA table_info(users)",
		"-- a comment\nSELECT 1",
		"/* block */ SELECT 1",
		"SELECT * FROM users;",
	}
	for _, q := range ok {
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
	}
	for _, q := range bad {
		if good, _ := isReadOnlyQuery(q); good {
			t.Errorf("expected refused: %q", q)
		}
	}
}

func TestRebind(t *testing.T) {
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
	cases := []struct {
		content any
		want    bool
	}{
		{map[string]any{"slow": true}, true},
		{map[string]any{"time": float64(150)}, true},
		{map[string]any{"time": float64(50)}, false},
		{map[string]any{"time": "120.5"}, true},
		{map[string]any{"time": "10"}, false},
		{map[string]any{}, false},
		{"not a map", false},
	}
	for i, c := range cases {
		if got := isSlowQuery(c.content); got != c.want {
			t.Errorf("case %d: got %v want %v", i, got, c.want)
		}
	}
}
