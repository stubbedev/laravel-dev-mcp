package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigEval(t *testing.T) {
	dir := t.TempDir()
	mkdir(t, filepath.Join(dir, "config"))
	write(t, filepath.Join(dir, ".env"), "APP_NAME=\"Acme\"\nDB_PORT=5544\nDEBUG_FLAG=true\n")
	write(t, filepath.Join(dir, "config", "app.php"), `<?php
return [
    'name' => env('APP_NAME', 'Laravel'),
    'fallback' => env('MISSING', 'def'),
    'debug' => env('DEBUG_FLAG', false),
    'port' => env('DB_PORT', 3306),
    'url' => 'http://' . env('HOST', 'localhost') . '/app',
    'mode' => env('APP_ENV', null) ? 'set' : 'unset',
    'nested' => ['a' => 1, 'b' => ['c' => true]],
    'list' => ['x', 'y', 'z'],
    'storage' => storage_path('framework'),
];
`)

	p := &Project{Root: dir}

	val, found, err := p.config("app.name")
	if err != nil || !found || val != "Acme" {
		t.Fatalf("app.name: got %v found=%v err=%v", val, found, err)
	}
	if v, _, _ := p.config("app.fallback"); v != "def" {
		t.Errorf("fallback default: got %v", v)
	}
	if v, _, _ := p.config("app.debug"); v != true {
		t.Errorf("debug cast bool: got %v (%T)", v, v)
	}
	if v, _, _ := p.config("app.port"); v != "5544" {
		t.Errorf("port from env: got %v", v)
	}
	if v, _, _ := p.config("app.url"); v != "http://localhost/app" {
		t.Errorf("concat: got %v", v)
	}
	if v, _, _ := p.config("app.mode"); v != "unset" {
		t.Errorf("ternary: got %v", v)
	}
	if v, _, _ := p.config("app.nested.b.c"); v != true {
		t.Errorf("nested navigate: got %v", v)
	}
	if v, _, _ := p.config("app.storage"); v != filepath.Join(dir, "storage", "framework") {
		t.Errorf("storage_path: got %v", v)
	}
	if _, found, _ := p.config("app.does.not.exist"); found {
		t.Errorf("missing key should not be found")
	}
}

func TestEvalLossyDetection(t *testing.T) {
	dir := t.TempDir()
	mkdir(t, filepath.Join(dir, "config"))
	write(t, filepath.Join(dir, "config", "static.php"), `<?php return ['a' => 1, 'b' => 2];`)
	write(t, filepath.Join(dir, "config", "dynamic.php"), `<?php $extra = []; return ['a' => 1, ...$extra];`)
	p := &Project{Root: dir}

	if _, _, _ = p.config("static.a"); p.evalLossy {
		t.Errorf("static config should not be lossy")
	}
	if _, _, _ = p.config("dynamic.a"); !p.evalLossy {
		t.Errorf("config with a spread must be flagged lossy (so the PHP fallback runs)")
	}
}

func mkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
