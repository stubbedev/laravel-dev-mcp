package main

import (
	"path/filepath"
	"sync"
	"testing"
)

// makeApp writes a minimal Laravel-ish app with a given app name and one
// composer package, returning its root.
func makeApp(t *testing.T, name, pkg string) string {
	t.Helper()
	dir := t.TempDir()
	mkdir(t, filepath.Join(dir, "config"))
	write(t, filepath.Join(dir, ".env"), "APP_NAME="+name+"\n")
	write(t, filepath.Join(dir, "config", "app.php"),
		`<?php return ['name' => env('APP_NAME', 'def')];`)
	write(t, filepath.Join(dir, "composer.lock"),
		`{"packages":[{"name":"`+pkg+`","version":"v1.0.0"}],"packages-dev":[]}`)
	return dir
}

// TestNoCrossContamination hammers two distinct roots concurrently and asserts
// each call only ever sees its own project's data. Run with -race.
func TestNoCrossContamination(t *testing.T) {
	a := makeApp(t, "Alpha", "vendor/alpha")
	b := makeApp(t, "Bravo", "vendor/bravo")

	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			root, wantName, wantPkg := a, "Alpha", "vendor/alpha"
			if i%2 == 1 {
				root, wantName, wantPkg = b, "Bravo", "vendor/bravo"
			}
			p := &Project{Root: root}
			if v, found, err := p.config("app.name"); err != nil || !found || v != wantName {
				t.Errorf("root %s: config name = %v (found=%v err=%v), want %s", root, v, found, err, wantName)
			}
			if p.Env("APP_NAME", "") != wantName {
				t.Errorf("root %s: env name mismatch", root)
			}
			if !p.hasPackage(wantPkg) {
				t.Errorf("root %s: missing own package %s", root, wantPkg)
			}
			// Must NOT see the other root's package.
			otherPkg := "vendor/bravo"
			if wantPkg == otherPkg {
				otherPkg = "vendor/alpha"
			}
			if p.hasPackage(otherPkg) {
				t.Errorf("root %s: leaked other root's package %s", root, otherPkg)
			}
		}(i)
	}
	wg.Wait()
}

// TestCacheInvalidation verifies an edited .env is re-read (size/mtime change
// busts the cache).
func TestCacheInvalidation(t *testing.T) {
	dir := t.TempDir()
	mkdir(t, filepath.Join(dir, "config"))
	write(t, filepath.Join(dir, "config", "app.php"),
		`<?php return ['name' => env('APP_NAME', 'def')];`)
	env := filepath.Join(dir, ".env")
	p := &Project{Root: dir}

	write(t, env, "APP_NAME=First\n")
	if v, _, _ := p.config("app.name"); v != "First" {
		t.Fatalf("got %v, want First", v)
	}
	write(t, env, "APP_NAME=SecondLonger\n") // different size → cache miss
	if v, _, _ := p.config("app.name"); v != "SecondLonger" {
		t.Fatalf("after edit got %v, want SecondLonger", v)
	}
}
