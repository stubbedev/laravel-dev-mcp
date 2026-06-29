package app

import (
	"path/filepath"
	"testing"
)

func TestResolveActionFile(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "composer.json"), `{"autoload":{"psr-4":{"App\\":"app/","App\\Http\\":"app/Http/"}}}`)
	mkdir(t, filepath.Join(dir, "app", "Http", "Controllers"))
	write(
		t,
		filepath.Join(dir, "app", "Http", "Controllers", "FooController.php"),
		"<?php\nclass FooController {\n    public function index() {}\n    public function __invoke() {}\n}\n",
	)

	p := &Project{Root: dir}
	psr4 := p.psr4Map()

	cases := map[string]string{
		`App\Http\Controllers\FooController@index`: "app/Http/Controllers/FooController.php:3", // longest prefix App\Http\ wins
		`App\Http\Controllers\FooController`:       "app/Http/Controllers/FooController.php:4", // invokable -> __invoke
		`Closure`:                                  "",
		`App\Nope\MissingController@x`:             "", // file does not exist -> still resolves path? no: scanFuncLine 0 -> returns rel
	}
	for action, want := range cases {
		if got := p.resolveActionFile(psr4, action); got != want {
			t.Errorf("resolveActionFile(%q) = %q, want %q", action, got, want)
		}
	}
}
