package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

	// envName selects which environment's variables resolve config and DB
	// settings. Empty means the live app (.env). A name like "testing" overlays
	// .env.<name> — and, for "testing", phpunit.xml's <env> entries — on top of
	// .env, so the db tools can reach the test database. Set per call from the
	// tool's `env` argument.
	envName string

	// evalLossy is set during a config eval when a dynamic construct (array
	// spread, unresolved function call) was skipped, meaning the static result
	// is incomplete and a PHP fallback is required for a faithful answer. It is
	// per-call state (Project is created fresh per tool call, used by one
	// goroutine) — reset at the start of each file eval.
	evalLossy bool
}

// newProject returns the Project for an app rooted at root, with the live
// environment selected.
func newProject(root string) *Project {
	return &Project{Root: root, envName: "", evalLossy: false}
}

// ── Parsed-file cache ─────────────────────────────────────────────────────────

type cacheEntry struct {
	modNano int64 // modtime unix-nanos
	size    int64
	val     any
}

// parsedFileCache maps an absolute path to its last parse result.
type parsedFileCache struct {
	mu      sync.RWMutex
	entries map[string]cacheEntry
}

// fileCache is process-wide on purpose: every tool call builds a fresh Project,
// so only a shared cache lets repeated calls skip re-parsing unchanged files.
//
//nolint:gochecknoglobals // the one process-wide cache; keyed by absolute path so roots never collide
var fileCache = &parsedFileCache{mu: sync.RWMutex{}, entries: map[string]cacheEntry{}}

// loadCached returns parse(file) for the absolute path, reusing a cached result
// while the file's modtime and size are unchanged. Safe for concurrent use; the
// cached value MUST be treated as read-only by callers. Keyed by absolute path,
// so distinct project roots never collide.
func loadCached[T any](path string, parse func([]byte) (T, error)) (T, error) {
	var zero T

	info, err := os.Stat(path)
	if err != nil {
		return zero, fmt.Errorf("load %s: %w", filepath.Base(path), err)
	}

	modNano, size := info.ModTime().UnixNano(), info.Size()

	fileCache.mu.RLock()

	entry, cached := fileCache.entries[path]

	fileCache.mu.RUnlock()

	if cached && entry.modNano == modNano && entry.size == size {
		if val, isT := entry.val.(T); isT {
			return val, nil
		}
	}

	raw, err := os.ReadFile(path) //nolint:gosec // G304: reading the project's own files is the point
	if err != nil {
		return zero, fmt.Errorf("load %s: %w", filepath.Base(path), err)
	}

	val, err := parse(raw)
	if err != nil {
		return zero, err
	}

	fileCache.mu.Lock()
	fileCache.entries[path] = cacheEntry{modNano: modNano, size: size, val: val}
	fileCache.mu.Unlock()

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

	cwd, err := os.Getwd()
	if err == nil {
		candidates = append(candidates, cwd)
	}

	// First, a candidate that is a Laravel app.
	for _, candidate := range candidates {
		if isLaravelRoot(candidate) {
			abs, _ := filepath.Abs(candidate)

			return newProject(abs), nil
		}
	}
	// Else the first existing directory (lets tools give a precise error).
	for _, candidate := range candidates {
		info, statErr := os.Stat(candidate)
		if statErr == nil && info.IsDir() {
			abs, _ := filepath.Abs(candidate)

			return nil, fmt.Errorf("%s %w", abs, errLaravelNotApp)
		}
	}

	return nil, errLaravelNoRoot
}

// Root resolution failures, worded as the fix the user should make.
var (
	errLaravelNotApp = errors.New(
		"does not look like a Laravel app (no artisan/composer.json); " +
			"set the MCP root or X-Mcp-Root header to the app directory",
	)
	errLaravelNoRoot = errors.New(
		"no Laravel project root found; run from the app directory, or set the MCP root / X-Mcp-Root header",
	)
)

// isLaravelRoot reports whether dir contains the artisan entrypoint.
func isLaravelRoot(dir string) bool {
	if dir == "" {
		return false
	}

	info, err := os.Stat(filepath.Join(dir, "artisan"))

	return err == nil && !info.IsDir()
}

// Env returns a .env value, falling back to def when missing or empty.
func (p *Project) Env(key, def string) string {
	if val, ok := p.envMap()[key]; ok && val != "" {
		return val
	}

	return def
}

func (p *Project) artisan() string { return filepath.Join(p.Root, "artisan") }
func (p *Project) path(rel ...string) string {
	return filepath.Join(slices.Concat([]string{p.Root}, rel)...)
}

// ── .env ─────────────────────────────────────────────────────────────────────

// envFile parses one dotenv file (cached, read-only). Missing file → empty map.
func (p *Project) envFile(name string) map[string]string {
	env, err := loadCached(p.path(name), func(raw []byte) (map[string]string, error) {
		return parseDotEnv(raw), nil
	})
	if err != nil {
		return map[string]string{}
	}

	return env
}

// envMap returns the project's effective environment (cached, read-only). For
// the default environment that is just .env. When envName is set, .env.<name>
// overlays .env and — for "testing" — phpunit.xml's <env> entries overlay both,
// mirroring how Laravel resolves config under tests. Only the project's own
// files are consulted, never the server's process environment, so resolution
// stays isolated per root.
func (p *Project) envMap() map[string]string {
	base := p.envFile(".env")
	if p.envName == "" {
		return base
	}

	merged := maps.Clone(base)
	maps.Copy(merged, p.envFile(".env."+p.envName))

	if p.envName == "testing" {
		maps.Copy(merged, p.phpunitEnv())
	}

	return merged
}

// phpunitEnv reads the <php><env name=… value=…> overrides from phpunit.xml (or
// phpunit.xml.dist) — the values Laravel applies when running the test suite.
func (p *Project) phpunitEnv() map[string]string {
	for _, name := range []string{"phpunit.xml", "phpunit.xml.dist"} {
		env, err := loadCached(p.path(name), parsePhpunitEnv)
		if err == nil && len(env) > 0 {
			return env
		}
	}

	return map[string]string{}
}

// phpunitDoc is the slice of phpunit.xml that carries env overrides.
type phpunitDoc struct {
	Php phpunitPHP `xml:"php"`
}

type phpunitPHP struct {
	Env []phpunitEnvVar `xml:"env"`
}

type phpunitEnvVar struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

func parsePhpunitEnv(raw []byte) (map[string]string, error) {
	var doc phpunitDoc

	err := xml.Unmarshal(raw, &doc)
	if err != nil {
		return nil, fmt.Errorf("parse phpunit env: %w", err)
	}

	out := make(map[string]string, len(doc.Php.Env))
	for _, e := range doc.Php.Env {
		out[e.Name] = e.Value
	}

	return out, nil
}

// dotenv scanner buffer: start small, but allow long values (inline certs,
// JSON blobs) up to a sane cap.
const (
	dotenvBufInit = 64 * 1024
	dotenvBufMax  = 1024 * 1024
)

func parseDotEnv(raw []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, dotenvBufInit), dotenvBufMax)

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
	PackagesDev []composerPackage `json:"packages-dev"` //nolint:tagliatelle // composer.lock's own key
}

// all returns the runtime and dev packages as one fresh slice. The lock is
// shared through the file cache, so this never appends into its slices.
func (lock *composerLock) all() []composerPackage {
	return slices.Concat(lock.Packages, lock.PackagesDev)
}

func (p *Project) composerLock() (*composerLock, error) {
	return loadCached(p.path("composer.lock"), func(raw []byte) (*composerLock, error) {
		var lock composerLock

		err := json.Unmarshal(raw, &lock)
		if err != nil {
			return nil, fmt.Errorf("parse composer.lock: %w", err)
		}

		return &lock, nil
	})
}

// hasPackage reports whether the named composer package is installed.
func (p *Project) hasPackage(name string) bool {
	lock, err := p.composerLock()
	if err != nil {
		return false
	}

	isName := func(pkg composerPackage) bool { return pkg.Name == name }

	return slices.ContainsFunc(lock.Packages, isName) || slices.ContainsFunc(lock.PackagesDev, isName)
}

// ── artisan exec ─────────────────────────────────────────────────────────────

// errArtisanFailed reads as "php artisan <args> failed: <stderr>".
var errArtisanFailed = errors.New("failed")

// runArtisan runs `php artisan <args...>` in the project root and returns
// trimmed stdout. On failure it returns an error including stderr.
func (p *Project) runArtisan(ctx context.Context, args ...string) (string, error) {
	//nolint:gosec // G204: running the user's configured php on their own artisan is the tool's job
	cmd := exec.CommandContext(ctx, cfg.PHPBin, slices.Concat([]string{p.artisan()}, args)...)
	cmd.Dir = p.Root

	var out, errb strings.Builder

	cmd.Stdout = &out
	cmd.Stderr = &errb

	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}

		return "", fmt.Errorf("php artisan %s %w: %s", strings.Join(args, " "), errArtisanFailed, msg)
	}

	return strings.TrimSpace(out.String()), nil
}
