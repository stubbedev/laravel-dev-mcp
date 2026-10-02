package app

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveActionFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	write(t, filepath.Join(dir, "composer.json"), `{"autoload":{"psr-4":{"App\\":"app/","App\\Http\\":"app/Http/"}}}`)
	mkdir(t, filepath.Join(dir, "app", "Http", "Controllers"))
	write(
		t,
		filepath.Join(dir, "app", "Http", "Controllers", "FooController.php"),
		"<?php\nclass FooController {\n    public function index() {}\n    public function __invoke() {}\n}\n",
	)

	proj := newProject(dir)
	psr4 := proj.psr4Map()

	cases := map[string]string{
		// Longest prefix App\Http\ wins.
		`App\Http\Controllers\FooController@index`: "app/Http/Controllers/FooController.php:3",
		// Invokable -> __invoke.
		`App\Http\Controllers\FooController`: "app/Http/Controllers/FooController.php:4",
		`Closure`:                            "",
		`App\Nope\MissingController@x`:       "", // no such file
	}
	for action, want := range cases {
		if got := proj.resolveActionFile(psr4, action); got != want {
			t.Errorf("resolveActionFile(%q) = %q, want %q", action, got, want)
		}
	}
}

// composer.json may map one PSR-4 prefix to several directories; the first that
// holds the class wins.
func TestResolveActionFilePSR4List(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	write(t, filepath.Join(dir, "composer.json"), `{"autoload":{"psr-4":{"App\\":["src/","app/"]}}}`)
	mkdir(t, filepath.Join(dir, "app", "Http"))
	write(
		t,
		filepath.Join(dir, "app", "Http", "BarController.php"),
		"<?php\nclass BarController {\n    public function show() {}\n}\n",
	)

	proj := newProject(dir)

	got := proj.resolveActionFile(proj.psr4Map(), `App\Http\BarController@show`)
	if want := "app/Http/BarController.php:3"; got != want {
		t.Errorf("resolveActionFile = %q, want %q", got, want)
	}
}

func TestTruncateUTF8(t *testing.T) {
	t.Parallel()

	if got := truncateUTF8("aé", 2); got != "a" {
		t.Errorf("truncateUTF8 split a rune: %q", got)
	}

	if got := truncateUTF8("abc", 5); got != "abc" {
		t.Errorf("truncateUTF8 short input = %q", got)
	}
}

// When PHP can't resolve a lossy config, the static value is still returned,
// with a note, instead of an error.
func TestResolveLossyConfigFallsBackToStatic(t *testing.T) {
	t.Parallel()

	proj := newProject(t.TempDir()) // no artisan here, so the PHP fallback fails
	proj.evalLossy = true

	val, found, note := proj.resolveLossyConfig(t.Context(), "app.name", "Static", true)
	if val != "Static" || !found {
		t.Errorf("resolveLossyConfig = %v, %v; want the static value", val, found)
	}

	if !strings.Contains(note, "config/app.php") {
		t.Errorf("note = %q, want it to name config/app.php", note)
	}
}

func TestConfigFileOf(t *testing.T) {
	t.Parallel()

	for key, want := range map[string]string{"app.name": "app", "database": "database", "a.b.c": "a"} {
		if got := configFileOf(key); got != want {
			t.Errorf("configFileOf(%q) = %q, want %q", key, got, want)
		}
	}
}
