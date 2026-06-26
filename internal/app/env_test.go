package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnvMapOverlay(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".env", "DB_DATABASE=app\nDB_USERNAME=root\n")
	write(".env.testing", "DB_DATABASE=app_testing\n")
	write("phpunit.xml", `<phpunit><php><env name="DB_HOST" value="127.0.0.1"/></php></phpunit>`)

	p := &Project{Root: dir}

	// Default environment: just .env.
	if got := p.envMap()["DB_DATABASE"]; got != "app" {
		t.Fatalf("default DB_DATABASE = %q, want app", got)
	}

	// testing: .env.testing overlays .env, phpunit.xml overlays both, base keys survive.
	p.envName = "testing"
	m := p.envMap()
	if m["DB_DATABASE"] != "app_testing" {
		t.Fatalf(".env.testing overlay: DB_DATABASE = %q", m["DB_DATABASE"])
	}
	if m["DB_HOST"] != "127.0.0.1" {
		t.Fatalf("phpunit overlay: DB_HOST = %q", m["DB_HOST"])
	}
	if m["DB_USERNAME"] != "root" {
		t.Fatalf("base key lost: DB_USERNAME = %q", m["DB_USERNAME"])
	}
}
