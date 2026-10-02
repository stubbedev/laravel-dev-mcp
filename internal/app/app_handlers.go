package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// noOutput stands in for an empty artisan/tinker result, so the caller can
// tell "ran, printed nothing" from a dropped response.
const noOutput = "(no output)"

func routes(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	proj, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}

	out, err := proj.runArtisan(ctx, "route:list", "--json")
	if err != nil {
		return toolResult{}, err
	}

	var all []map[string]any

	err = json.Unmarshal([]byte(out), &all)
	if err != nil {
		return toolResult{}, fmt.Errorf("could not parse route:list output: %w", err)
	}

	all = filterRoutes(all, args)

	// Best-effort: resolve each controller action to a clickable file:line so the
	// caller can jump straight to the handler. Closures/unresolvable actions are
	// left untouched.
	psr4 := proj.psr4Map()
	for _, route := range all {
		if loc := proj.resolveActionFile(psr4, fmt.Sprint(route["action"])); loc != "" {
			route["action_file"] = loc
		}
	}

	return jsonResult(ctx, all), nil
}

// filterRoutes keeps the route:list rows matching the path/name/method
// arguments (case-insensitive substring matches). It filters in place.
func filterRoutes(all []map[string]any, args map[string]any) []map[string]any {
	pathF := strings.ToLower(argString(args, keyPath))
	methodF := strings.ToUpper(argString(args, "method"))
	nameF := strings.ToLower(argString(args, "name"))

	if pathF == "" && methodF == "" && nameF == "" {
		return all
	}

	matches := func(route map[string]any, field, want string, fold func(string) string) bool {
		return want == "" || strings.Contains(fold(fmt.Sprint(route[field])), want)
	}

	filtered := all[:0]
	for _, route := range all {
		if matches(route, "uri", pathF, strings.ToLower) &&
			matches(route, "name", nameF, strings.ToLower) &&
			matches(route, "method", methodF, strings.ToUpper) {
			filtered = append(filtered, route)
		}
	}

	return filtered
}

// psr4Dirs is a PSR-4 target, which composer.json allows as one directory or a
// list of them.
type psr4Dirs []string

func (d *psr4Dirs) UnmarshalJSON(data []byte) error {
	var one string

	err := json.Unmarshal(data, &one)
	if err == nil {
		*d = psr4Dirs{one}

		return nil
	}

	var many []string

	err = json.Unmarshal(data, &many)
	if err != nil {
		return fmt.Errorf("psr-4 target is neither a directory nor a list: %w", err)
	}

	*d = many

	return nil
}

// composerAutoload is one autoload section of composer.json.
type composerAutoload struct {
	PSR4 map[string]psr4Dirs `json:"psr-4"` //nolint:tagliatelle // composer.json key
}

// composerManifest is the slice of composer.json psr4Map reads.
type composerManifest struct {
	Autoload    composerAutoload `json:"autoload"`
	AutoloadDev composerAutoload `json:"autoload-dev"` //nolint:tagliatelle // composer.json key
}

// psr4Map reads composer.json's PSR-4 autoload prefixes (both autoload and
// autoload-dev) into namespace-prefix → relative dirs. Empty on any error.
func (p *Project) psr4Map() map[string]psr4Dirs {
	raw, err := os.ReadFile(p.path("composer.json"))
	if err != nil {
		return nil
	}

	var doc composerManifest
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}

	out := map[string]psr4Dirs{}
	maps.Copy(out, doc.Autoload.PSR4)
	maps.Copy(out, doc.AutoloadDev.PSR4)

	return out
}

// resolveActionFile maps a route action ("App\\Http\\Controllers\\X@method" or
// an invokable "App\\Http\\Controllers\\X") to "relative/path.php:line" using the
// PSR-4 map. Returns "" for closures or anything it can't resolve.
func (p *Project) resolveActionFile(psr4 map[string]psr4Dirs, action string) string {
	if action == "" || !strings.Contains(action, "\\") {
		return ""
	}

	class, method := action, "__invoke"
	if c, m, ok := strings.CutLast(action, "@"); ok {
		class, method = c, m
	}

	prefix := longestPSR4Prefix(psr4, class)
	if prefix == "" {
		return ""
	}

	// Within the prefix, the first directory that holds the file wins.
	file := strings.ReplaceAll(class[len(prefix):], "\\", "/") + ".php"
	for _, dir := range psr4[prefix] {
		rel := filepath.Join(dir, file)

		info, err := os.Stat(p.path(rel))
		if err != nil || info.IsDir() {
			continue
		}

		if line := scanFuncLine(p.path(rel), method); line > 0 {
			return rel + ":" + strconv.Itoa(line)
		}

		return rel
	}

	return ""
}

// longestPSR4Prefix returns the longest PSR-4 prefix class falls under (e.g.
// App\Http\ over App\), or "" when none does.
func longestPSR4Prefix(psr4 map[string]psr4Dirs, class string) string {
	var best string

	for prefix := range psr4 {
		if strings.HasPrefix(class, prefix) && len(prefix) > len(best) {
			best = prefix
		}
	}

	return best
}

// scanFuncLine returns the 1-based line of `function <name>(` in a file, or 0.
func scanFuncLine(path, name string) int {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: reading the user's own project source is the point
	if err != nil {
		return 0
	}

	needle := "function " + name + "("

	i := 0
	for line := range strings.Lines(string(raw)) {
		i++
		if strings.Contains(line, needle) {
			return i
		}
	}

	return 0
}

// configFileOf returns the config file a dotted key lives in ("app" for
// "app.name").
func configFileOf(key string) string {
	file, _, _ := strings.Cut(key, ".")

	return file
}

func configValue(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	proj, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}

	key := argString(args, "key")
	if key == "" {
		return configListing(ctx, proj,
			"Pass a dotted key, e.g. \"app.name\" or \"database.connections\", "+
				"or a file name like \"app\" for the whole file."), nil
	}

	val, found, err := proj.config(key)
	if errors.Is(err, fs.ErrNotExist) {
		// No such config file: the same answer as an unset key, plus what
		// does exist so the next call can get it right.
		return configListing(ctx, proj,
			fmt.Sprintf("config(%q) is not set: there is no config/%s.php.", key, configFileOf(key))), nil
	}

	if err != nil {
		return toolResult{}, err
	}

	val, found, note := proj.resolveLossyConfig(ctx, key, val, found)
	if !found {
		return textResult(fmt.Sprintf("config(%q) is not set.", key)), nil
	}

	res := jsonResult(ctx, redactConfigValue(key, val))
	if note != "" {
		res.Content = append(res.Content, contentBlock{Type: contentText, Text: note})
	}

	return res, nil
}

// configListing answers with the app's config files and a note.
func configListing(ctx context.Context, proj *Project, note string) toolResult {
	files, err := proj.configFiles()
	if err != nil {
		files = []string{} // no config dir: an empty listing still answers the question
	}

	return jsonResult(ctx, map[string]any{"config_files": files, keyNote: note})
}

// resolveLossyConfig re-resolves a key through PHP when static eval skipped
// dynamic constructs (array spreads, etc.), rather than returning a partial
// answer. When PHP can't run, the static value is still the best answer
// available, returned with a note saying so.
func (p *Project) resolveLossyConfig(ctx context.Context, key string, val any, found bool) (any, bool, string) {
	if !p.evalLossy {
		return val, found, ""
	}

	phpVal, phpFound, phpErr := p.configViaPHP(ctx, key)
	if phpErr == nil {
		return phpVal, phpFound, ""
	}

	return val, found, fmt.Sprintf(
		"Read statically: config/%s.php uses dynamic constructs or PHP syntax newer than 8.1, "+
			"so parts of this value may be missing. PHP could not resolve it (%v); "+
			"make sure `php` runs in the app (LARAVEL_MCP_PHP) for the exact value.",
		configFileOf(key),
		phpErr,
	)
}

// redactConfigValue masks secrets (APP_KEY, passwords, API secrets, …) so they
// never reach the client.
func redactConfigValue(key string, val any) any {
	if !isNonEmptyScalar(val) {
		return redactValue(val)
	}

	if sensitiveKeyRe.MatchString(lastSegment(key)) {
		return redacted
	}

	return val
}

// errRouteNotFound completes `named route "x" not found`.
var errRouteNotFound = errors.New("not found")

func absoluteURL(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	proj, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}

	base := strings.TrimRight(proj.Env("APP_URL", "http://localhost"), "/")

	if name := argString(args, "route"); name != "" {
		out, rerr := proj.runArtisan(ctx, "route:list", "--json")
		if rerr != nil {
			return toolResult{}, rerr
		}

		var all []map[string]any

		_ = json.Unmarshal([]byte(out), &all)
		for _, route := range all {
			if fmt.Sprint(route["name"]) == name {
				uri := strings.TrimLeft(fmt.Sprint(route["uri"]), "/")

				return textResult(base + "/" + uri), nil
			}
		}

		return toolResult{}, fmt.Errorf("named route %q %w", name, errRouteNotFound)
	}

	path := strings.TrimLeft(argString(args, keyPath), "/")

	return textResult(base + "/" + path), nil
}

// docsUserAgent mirrors laravel/boost's hosted-docs request so the API treats
// us identically.
const docsUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:140.0) Gecko/20100101 " +
	"Firefox/140.0 Laravel Boost"

// docs_search limits: the token budget the docs API is asked to fill (default
// and cap), and the most response we read back.
const (
	docsDefaultTokenLimit = 3000
	docsMaxTokenLimit     = 1_000_000
	docsMaxResponseBytes  = 4 << 20
)

var (
	errQueriesRequired  = errors.New("queries is required")
	errDocsSearchStatus = errors.New("docs search returned")
)

// docsPackage is one package of context for the docs API (name + major.x), like
// Boost's Roster.
type docsPackage struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

func docsSearch(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	queries := docsQueries(args)
	if len(queries) == 0 {
		return toolResult{}, errQueriesRequired
	}

	body, err := json.Marshal(map[string]any{
		"queries":     queries,
		"packages":    docsPackages(ctx, req, args),
		"token_limit": argClampInt(args, "token_limit", docsDefaultTokenLimit, docsMaxTokenLimit),
		"format":      "markdown",
	})
	if err != nil {
		return toolResult{}, fmt.Errorf("encode docs search request: %w", err)
	}

	raw, err := fetchDocs(ctx, body)
	if err != nil {
		return toolResult{}, err
	}

	return textResult(raw), nil
}

// docsQueries collects `queries` plus a single `query`, dropping blanks and the
// match-everything "*".
func docsQueries(args map[string]any) []string {
	queries := argStrSlice(args, "queries")
	if q := argString(args, "query"); q != "" {
		queries = append(queries, q)
	}

	cleaned := queries[:0]
	for _, q := range queries {
		if q = strings.TrimSpace(q); q != "" && q != "*" {
			cleaned = append(cleaned, q)
		}
	}

	return cleaned
}

// docsPackages lists the project's composer packages as docs context,
// optionally narrowed to `packages`. Docs search works without a Laravel
// project, so any failure here just means no package context.
func docsPackages(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) []docsPackage {
	proj, err := resolveProject(ctx, req)
	if err != nil {
		return nil
	}

	lock, err := proj.composerLock()
	if err != nil {
		return nil
	}

	filter := map[string]bool{}
	for _, name := range argStrSlice(args, "packages") {
		filter[name] = true
	}

	var packages []docsPackage

	for _, lockPkg := range lock.all() {
		if len(filter) > 0 && !filter[lockPkg.Name] {
			continue
		}

		if major := majorX(lockPkg.Version); major != "" {
			packages = append(packages, docsPackage{Name: lockPkg.Name, Version: major})
		}
	}

	return packages
}

// fetchDocs posts one search to the docs API and returns its response body.
func fetchDocs(ctx context.Context, body []byte) (string, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.DocsURL+"/api/docs", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build docs search request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", docsUserAgent)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("docs search request failed: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, docsMaxResponseBytes))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w %s: %s", errDocsSearchStatus, resp.Status, strings.TrimSpace(string(raw)))
	}

	return string(raw), nil
}

// majorX turns a composer version ("v11.9.0", "8.2.1") into Boost's "11.x"
// form. Returns "" for non-numeric versions (dev-*, branch aliases).
func majorX(v string) string {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")

	major, _, _ := strings.Cut(v, ".")
	if major == "" {
		return ""
	}

	for _, c := range major {
		if c < '0' || c > '9' {
			return ""
		}
	}

	return major + ".x"
}

// artisanAllowlist is the set of read-only artisan commands the `artisan` tool
// may run, sorted. Anything that mutates state is intentionally excluded
// (schedule:test, for one, runs the chosen task) — use `tinker` for that.
func artisanAllowlist() []string {
	return []string{
		"about",
		"channel:list",
		"config:show",
		"db:monitor",
		"db:show",
		"db:table",
		"env",
		"event:list",
		"migrate:status",
		"model:show",
		"permission:show",
		"queue:failed",
		"queue:monitor",
		"route:list",
		"schedule:list",
	}
}

var (
	errCommandRequired = errors.New("command is required")
	errCodeRequired    = errors.New("code is required")
)

func artisan(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	command := strings.TrimSpace(argString(args, "command"))
	if command == "" {
		return toolResult{}, errCommandRequired
	}

	allowed := artisanAllowlist()
	if !slices.Contains(allowed, command) {
		return toolErrResult(fmt.Sprintf(
			"Refused: %q is not an allowed read-only artisan command. Allowed: %s. For arbitrary commands use `tinker`.",
			command,
			strings.Join(allowed, ", "),
		)), nil
	}

	proj, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}

	out, err := proj.runArtisan(ctx, slices.Concat([]string{command}, argStrSlice(args, "args"))...)
	if err != nil {
		return toolResult{}, err
	}

	if strings.TrimSpace(out) == "" {
		return textResult(noOutput), nil
	}

	return textResult(out), nil
}

func tinker(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	code := argString(args, "code")
	if code == "" {
		return toolResult{}, errCodeRequired
	}

	proj, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}

	if !proj.hasPackage("laravel/tinker") {
		return toolErrResult(
			"laravel/tinker is not installed. Install it (composer require --dev laravel/tinker) to use this tool.",
		), nil
	}

	out, err := proj.runArtisan(ctx, "tinker", "--execute="+code)
	if err != nil {
		return toolResult{}, err
	}

	if strings.TrimSpace(out) == "" {
		return textResult(noOutput), nil
	}

	return textResult(out), nil
}
