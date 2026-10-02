package app

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestConfigEval(t *testing.T) {
	t.Parallel()

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

	proj := newProject(dir)

	val, found, err := proj.config("app.name")
	if err != nil || !found || val != "Acme" {
		t.Fatalf("app.name: got %v found=%v err=%v", val, found, err)
	}

	want := map[string]any{
		"fallback":   "def",                                      // env() default
		"debug":      true,                                       // env() cast to bool
		"port":       "5544",                                     // value from .env
		"url":        "http://localhost/app",                     // concat
		"mode":       "unset",                                    // ternary
		"nested.b.c": true,                                       // nested navigate
		"storage":    filepath.Join(dir, "storage", "framework"), // storage_path
	}
	for key, wantVal := range want {
		if got, _, _ := proj.config("app." + key); !reflect.DeepEqual(got, wantVal) {
			t.Errorf("app.%s = %#v, want %#v", key, got, wantVal)
		}
	}

	if _, found, _ := proj.config("app.does.not.exist"); found {
		t.Error("missing key should not be found")
	}
}

func TestEvalLossyDetection(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mkdir(t, filepath.Join(dir, "config"))
	write(t, filepath.Join(dir, "config", "static.php"), `<?php return ['a' => 1, 'b' => 2];`)
	write(t, filepath.Join(dir, "config", "dynamic.php"), `<?php $extra = []; return ['a' => 1, ...$extra];`)
	proj := newProject(dir)

	if _, _, _ = proj.config("static.a"); proj.evalLossy {
		t.Error("static config should not be lossy")
	}

	if _, _, _ = proj.config("dynamic.a"); !proj.evalLossy {
		t.Error("config with a spread must be flagged lossy (so the PHP fallback runs)")
	}
}

func mkdir(t *testing.T, p string) {
	t.Helper()

	err := os.MkdirAll(p, 0o750)
	if err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, p, content string) {
	t.Helper()

	err := os.WriteFile(p, []byte(content), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

// Syntax past the parser's 8.1 grammar must mark the eval lossy so the config
// tool resolves the key through PHP instead of returning a mangled value.
func TestConfigNewerSyntaxIsLossy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		src   string
		lossy bool
	}{
		{name: "php 8.1", src: `<?php return ['name' => env('APP_NAME', 'x')];`, lossy: false},
		{
			name:  "php 8.5 pipe",
			src:   `<?php return ['name' => env('APP_NAME', 'x') |> trim(...), 'b' => 'c'];`,
			lossy: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			mkdir(t, filepath.Join(dir, "config"))
			write(t, filepath.Join(dir, "config", "app.php"), test.src)

			proj := newProject(dir)

			_, _, err := proj.config("app.name")
			if err != nil {
				t.Fatalf("config: %v", err)
			}

			if proj.evalLossy != test.lossy {
				t.Errorf("evalLossy = %v, want %v", proj.evalLossy, test.lossy)
			}
		})
	}
}

// Expressions lifted from Laravel's stock config files, which must evaluate
// statically (and exactly) rather than fall through to PHP.
func TestConfigStockExpressions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mkdir(t, filepath.Join(dir, "config"))
	write(
		t,
		filepath.Join(dir, ".env"),
		"APP_NAME=\"My App\"\nAPP_ENV=local\nLOG_STACK=single,daily\nAPP_URL=https://shop.test:8443/\n",
	)
	write(t, filepath.Join(dir, "config", "stock.php"), `<?php
use Illuminate\Support\Str;
return [
    'cache_prefix' => env('CACHE_PREFIX', Str::slug((string) env('APP_NAME', 'laravel')).'-cache-'),
    'redis_prefix' => env('REDIS_PREFIX', Str::slug(env('APP_NAME', 'laravel'), '_').'_database_'),
    'log_stack' => explode(',', (string) env('LOG_STACK', 'single')),
    'url' => rtrim(env('APP_URL', 'http://localhost'), '/').'/storage',
    'host' => env('MAIL_EHLO_DOMAIN', parse_url((string) env('APP_URL', 'http://localhost'), PHP_URL_HOST)),
    'options' => array_filter(['ca' => env('MYSQL_ATTR_SSL_CA'), 'keep' => 'x']),
    'is_local' => env('APP_ENV') ? 'yes' : 'no',
    'not_debug' => ! env('APP_DEBUG', false),
    'lifetime' => (int) env('SESSION_LIFETIME', '120'),
    'missing' => 'pre-'.env('NOPE').'-post',
];
`)

	proj := newProject(dir)

	want := map[string]any{
		"cache_prefix": "my-app-cache-",
		"redis_prefix": "my_app_database_",
		"log_stack":    []any{driverSingle, driverDaily},
		"url":          "https://shop.test:8443/storage",
		"host":         "shop.test",
		"options":      map[string]any{"keep": "x"},
		"is_local":     "yes",
		"not_debug":    true,
		"lifetime":     int64(120),
		"missing":      "pre--post",
	}
	for key, wantVal := range want {
		got, found, err := proj.config("stock." + key)
		if err != nil || !found {
			t.Errorf("stock.%s: found=%v err=%v", key, found, err)

			continue
		}

		if !reflect.DeepEqual(got, wantVal) {
			t.Errorf("stock.%s = %#v, want %#v", key, got, wantVal)
		}
	}

	if proj.evalLossy {
		t.Error("evalLossy = true, want every stock expression resolved statically")
	}
}

func TestConfigUnknownExpressionIsLossy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mkdir(t, filepath.Join(dir, "config"))
	write(t, filepath.Join(dir, "config", "app.php"), `<?php return ['tz' => Carbon::now()->timezoneName, 'b' => 'c'];`)

	proj := newProject(dir)

	_, _, err := proj.config("app.b")
	if err != nil {
		t.Fatal(err)
	}

	if !proj.evalLossy {
		t.Error("evalLossy = false for a method call, want true")
	}
}

func TestStrSlug(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in, sep, want string
		ok            bool
	}{
		{in: "Laravel", sep: "_", want: "laravel", ok: true},
		{in: "My App", sep: "-", want: "my-app", ok: true},
		{in: "My_App  Name", sep: "-", want: "my-app-name", ok: true},
		{in: "my-app", sep: "_", want: "my_app", ok: true},
		{in: "user@host!", sep: "-", want: "user-at-host", ok: true},
		{in: "--Edge--", sep: "-", want: "edge", ok: true},
		{in: "Café", sep: "-", want: "", ok: false},
	}
	for _, test := range tests {
		got, ok := strSlug(test.in, test.sep)
		if ok != test.ok || got != test.want {
			t.Errorf("strSlug(%q, %q) = %q, %v; want %q, %v", test.in, test.sep, got, ok, test.want, test.ok)
		}
	}
}
