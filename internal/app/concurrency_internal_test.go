package app

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
	t.Parallel()

	alphaRoot := makeApp(t, "Alpha", "vendor/alpha")
	bravoRoot := makeApp(t, "Bravo", "vendor/bravo")

	var group sync.WaitGroup
	for i := range 200 {
		group.Go(func() {
			root, wantName, wantPkg := alphaRoot, "Alpha", "vendor/alpha"
			if i%2 == 1 {
				root, wantName, wantPkg = bravoRoot, "Bravo", "vendor/bravo"
			}

			checkOwnProject(t, root, wantName, wantPkg)
		})
	}

	group.Wait()
}

// checkOwnProject asserts root resolves to its own app name and package, and
// never to the other root's package.
func checkOwnProject(t *testing.T, root, wantName, wantPkg string) {
	t.Helper()

	proj := newProject(root)

	val, found, err := proj.config("app.name")
	if err != nil || !found || val != wantName {
		t.Errorf("root %s: config name = %v (found=%v err=%v), want %s", root, val, found, err, wantName)
	}

	if proj.Env("APP_NAME", "") != wantName {
		t.Errorf("root %s: env name mismatch", root)
	}

	if !proj.hasPackage(wantPkg) {
		t.Errorf("root %s: missing own package %s", root, wantPkg)
	}
	// Must NOT see the other root's package.
	otherPkg := "vendor/bravo"
	if wantPkg == otherPkg {
		otherPkg = "vendor/alpha"
	}

	if proj.hasPackage(otherPkg) {
		t.Errorf("root %s: leaked other root's package %s", root, otherPkg)
	}
}

// TestCacheInvalidation verifies an edited .env is re-read (size/mtime change
// busts the cache).
func TestCacheInvalidation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mkdir(t, filepath.Join(dir, "config"))
	write(t, filepath.Join(dir, "config", "app.php"),
		`<?php return ['name' => env('APP_NAME', 'def')];`)
	env := filepath.Join(dir, ".env")
	proj := newProject(dir)

	write(t, env, "APP_NAME=First\n")

	if val, _, _ := proj.config("app.name"); val != "First" {
		t.Fatalf("got %v, want First", val)
	}

	write(t, env, "APP_NAME=SecondLonger\n") // different size → cache miss

	if val, _, _ := proj.config("app.name"); val != "SecondLonger" {
		t.Fatalf("after edit got %v, want SecondLonger", val)
	}
}
