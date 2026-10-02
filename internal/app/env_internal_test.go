package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnvMapOverlay(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	write := func(name, body string) {
		err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}
	write(".env", "DB_DATABASE=app\nDB_USERNAME=root\n")
	write(".env.testing", "DB_DATABASE=app_testing\n")
	write("phpunit.xml", `<phpunit><php><env name="DB_HOST" value="127.0.0.1"/></php></phpunit>`)

	proj := newProject(dir)

	// Default environment: just .env.
	if got := proj.envMap()["DB_DATABASE"]; got != "app" {
		t.Fatalf("default DB_DATABASE = %q, want app", got)
	}

	// testing: .env.testing overlays .env, phpunit.xml overlays both, base keys survive.
	proj.envName = "testing"

	env := proj.envMap()
	if env["DB_DATABASE"] != "app_testing" {
		t.Fatalf(".env.testing overlay: DB_DATABASE = %q", env["DB_DATABASE"])
	}

	if env["DB_HOST"] != "127.0.0.1" {
		t.Fatalf("phpunit overlay: DB_HOST = %q", env["DB_HOST"])
	}

	if env["DB_USERNAME"] != "root" {
		t.Fatalf("base key lost: DB_USERNAME = %q", env["DB_USERNAME"])
	}
}
