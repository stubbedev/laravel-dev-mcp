package main

import "testing"

func TestParseDotEnv(t *testing.T) {
	content := "APP_NAME=Laravel\n" +
		"# comment\n" +
		"DB_PASSWORD=\"se cret\"\n" +
		"DB_DATABASE='mydb'\n" +
		"EMPTY=\n" +
		"NOEQ\n"
	env := parseDotEnv([]byte(content))
	checks := map[string]string{
		"APP_NAME":    "Laravel",
		"DB_PASSWORD": "se cret",
		"DB_DATABASE": "mydb",
		"EMPTY":       "",
	}
	for k, want := range checks {
		if env[k] != want {
			t.Errorf("env[%q] = %q, want %q", k, env[k], want)
		}
	}
	if _, ok := env["NOEQ"]; ok {
		t.Errorf("NOEQ should be skipped")
	}
}

func TestParseLogEntries(t *testing.T) {
	raw := "[2024-01-02 15:04:05] local.INFO: started\n" +
		"[2024-01-02 15:04:06] local.ERROR: boom\n" +
		"#0 /app/foo.php(10): bar()\n" +
		"#1 {main}\n" +
		"[2024-01-02 15:04:07] local.WARNING: careful\n"
	entries := parseLogEntries(raw)
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}
	if entries[1].Level != "ERROR" {
		t.Errorf("entry 1 level = %q, want ERROR", entries[1].Level)
	}
	if entries[1].Channel != "local" {
		t.Errorf("entry 1 channel = %q, want local", entries[1].Channel)
	}
	if got := entries[1].Message; got == "boom" || len(got) <= len("boom") {
		t.Errorf("entry 1 message should include stack trace, got %q", got)
	}
}
