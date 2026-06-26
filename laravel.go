package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Project is a resolved Laravel application root for one tool call. It holds no
// parsed state itself — .env, composer.lock and config/*.php are read through
// the process-wide mtime cache (loadCached), keyed by absolute path, so two
// clients pointed at different roots never share data and an edited file is
// always re-read.
type Project struct {
	Root string // absolute filesystem path to the Laravel app

	// evalLossy is set during a config eval when a dynamic construct (array
	// spread, unresolved function call) was skipped, meaning the static result
	// is incomplete and a PHP fallback is required for a faithful answer. It is
	// per-call state (Project is created fresh per tool call, used by one
	// goroutine) — reset at the start of each file eval.
	evalLossy bool
}

// ── Parsed-file cache ─────────────────────────────────────────────────────────

type cacheEntry struct {
	mod  int64 // modtime unix-nanos
	size int64
	val  any
}

var (
	fileCacheMu sync.RWMutex
	fileCache   = map[string]cacheEntry{}
)

// loadCached returns parse(file) for the absolute path, reusing a cached result
// while the file's modtime and size are unchanged. Safe for concurrent use; the
// cached value MUST be treated as read-only by callers. Keyed by absolute path,
// so distinct project roots never collide.
func loadCached(path string, parse func([]byte) (any, error)) (any, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	mod, size := fi.ModTime().UnixNano(), fi.Size()

	fileCacheMu.RLock()
	e, ok := fileCache[path]
	fileCacheMu.RUnlock()
	if ok && e.mod == mod && e.size == size {
		return e.val, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	val, err := parse(raw)
	if err != nil {
		return nil, err
	}
	fileCacheMu.Lock()
	fileCache[path] = cacheEntry{mod: mod, size: size, val: val}
	fileCacheMu.Unlock()
	return val, nil
}

// resolveProject picks the Laravel project for the in-flight call from the MCP
// roots (header-pinned or session), falling back to the process working
// directory. It prefers a root that actually looks like a Laravel app.
func resolveProject(ctx context.Context, req *mcp.CallToolRequest) (*Project, error) {
	var candidates []string
	for _, r := range resolveRoots(ctx, req) {
		if p := r.path(); p != "" {
			candidates = append(candidates, p)
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, cwd)
	}

	// First, a candidate that is a Laravel app.
	for _, c := range candidates {
		if isLaravelRoot(c) {
			abs, _ := filepath.Abs(c)
			return &Project{Root: abs}, nil
		}
	}
	// Else the first existing directory (lets tools give a precise error).
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			abs, _ := filepath.Abs(c)
			return nil, fmt.Errorf(
				"%s does not look like a Laravel app (no artisan/composer.json); set the MCP root or X-Mcp-Root header to the app directory",
				abs,
			)
		}
	}
	return nil, errors.New("no Laravel project root found; run from the app directory, or set the MCP root / X-Mcp-Root header")
}

// isLaravelRoot reports whether dir contains the artisan entrypoint.
func isLaravelRoot(dir string) bool {
	if dir == "" {
		return false
	}
	if fi, err := os.Stat(filepath.Join(dir, "artisan")); err == nil && !fi.IsDir() {
		return true
	}
	return false
}

func (p *Project) artisan() string { return filepath.Join(p.Root, "artisan") }
func (p *Project) path(rel ...string) string {
	return filepath.Join(append([]string{p.Root}, rel...)...)
}

// ── .env ─────────────────────────────────────────────────────────────────────

// envMap returns the project's parsed .env (cached, read-only). Missing file →
// empty map. Only the project's own .env is consulted — never the server's
// process environment — so config resolution stays isolated per root.
func (p *Project) envMap() map[string]string {
	v, err := loadCached(p.path(".env"), func(b []byte) (any, error) {
		return parseDotEnv(b), nil
	})
	if err != nil {
		return map[string]string{}
	}
	m, _ := v.(map[string]string)
	return m
}

// Env returns a .env value, falling back to def when missing or empty.
func (p *Project) Env(key, def string) string {
	if v, ok := p.envMap()[key]; ok && v != "" {
		return v
	}
	return def
}

func parseDotEnv(raw []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rawKey, rawVal, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key := strings.TrimSpace(rawKey)
		val := strings.TrimSpace(rawVal)
		// Strip surrounding quotes (Laravel supports "..." and '...').
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		out[key] = val
	}
	return out
}

// ── composer ─────────────────────────────────────────────────────────────────

type composerPackage struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type composerLock struct {
	Packages    []composerPackage `json:"packages"`
	PackagesDev []composerPackage `json:"packages-dev"`
}

func (p *Project) composerLock() (*composerLock, error) {
	v, err := loadCached(p.path("composer.lock"), func(raw []byte) (any, error) {
		var cl composerLock
		if err := json.Unmarshal(raw, &cl); err != nil {
			return nil, err
		}
		return &cl, nil
	})
	if err != nil {
		return nil, err
	}
	cl, ok := v.(*composerLock)
	if !ok {
		return nil, errors.New("composer.lock cache type mismatch")
	}
	return cl, nil
}

// hasPackage reports whether the named composer package is installed.
func (p *Project) hasPackage(name string) bool {
	cl, err := p.composerLock()
	if err != nil {
		return false
	}
	for _, pkg := range append(cl.Packages, cl.PackagesDev...) {
		if pkg.Name == name {
			return true
		}
	}
	return false
}

// ── artisan exec ─────────────────────────────────────────────────────────────

// runArtisan runs `php artisan <args...>` in the project root and returns
// trimmed stdout. On failure it returns an error including stderr.
func (p *Project) runArtisan(ctx context.Context, args ...string) (string, error) {
	full := append([]string{p.artisan()}, args...)
	cmd := exec.CommandContext(ctx, cfg.PHPBin, full...)
	cmd.Dir = p.Root
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("php artisan %s failed: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(out.String()), nil
}
