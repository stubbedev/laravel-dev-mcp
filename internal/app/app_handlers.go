package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func routes(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	p, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}
	out, err := p.runArtisan(ctx, "route:list", "--json")
	if err != nil {
		return toolResult{}, err
	}
	var all []map[string]any
	if err := json.Unmarshal([]byte(out), &all); err != nil {
		return toolResult{}, fmt.Errorf("could not parse route:list output: %w", err)
	}

	pathF := strings.ToLower(argString(args, "path"))
	methodF := strings.ToUpper(argString(args, "method"))
	nameF := strings.ToLower(argString(args, "name"))
	if pathF != "" || methodF != "" || nameF != "" {
		filtered := all[:0]
		for _, r := range all {
			if pathF != "" && !strings.Contains(strings.ToLower(fmt.Sprint(r["uri"])), pathF) {
				continue
			}
			if nameF != "" && !strings.Contains(strings.ToLower(fmt.Sprint(r["name"])), nameF) {
				continue
			}
			if methodF != "" && !strings.Contains(strings.ToUpper(fmt.Sprint(r["method"])), methodF) {
				continue
			}
			filtered = append(filtered, r)
		}
		all = filtered
	}
	return jsonResult(ctx, all), nil
}

func configValue(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	p, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}
	key := argString(args, "key")
	if key == "" {
		files, ferr := p.configFiles()
		if ferr != nil {
			return toolResult{}, ferr
		}
		return jsonResult(ctx, map[string]any{
			"config_files": files,
			"note":         "Pass a dotted key, e.g. \"app.name\" or \"database.connections\", or a file name like \"app\" for the whole file.",
		}), nil
	}
	val, found, err := p.config(key)
	if err != nil {
		return toolResult{}, err
	}
	// If static eval skipped dynamic constructs (array spreads, etc.), resolve
	// the key faithfully via PHP rather than returning a partial answer.
	if p.evalLossy {
		pv, pfound, perr := p.configViaPHP(ctx, key)
		if perr != nil {
			return toolErrResult(fmt.Sprintf(
				"config(%q) uses dynamic constructs (e.g. array spreads) that need PHP to resolve fully, and the PHP fallback failed: %v. Ensure `php` runs in the app (LARAVEL_MCP_PHP).",
				key,
				perr,
			)), nil
		}
		val, found = pv, pfound
	}
	if !found {
		return textResult(fmt.Sprintf("config(%q) is not set.", key)), nil
	}
	// Never surface secrets (APP_KEY, passwords, API secrets, …).
	if isNonEmptyScalar(val) {
		if sensitiveKeyRe.MatchString(lastSegment(key)) {
			val = redacted
		}
	} else {
		val = redactValue(val)
	}
	return jsonResult(ctx, val), nil
}

func absoluteURL(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	p, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}
	base := strings.TrimRight(p.Env("APP_URL", "http://localhost"), "/")

	if name := argString(args, "route"); name != "" {
		out, rerr := p.runArtisan(ctx, "route:list", "--json")
		if rerr != nil {
			return toolResult{}, rerr
		}
		var all []map[string]any
		_ = json.Unmarshal([]byte(out), &all)
		for _, r := range all {
			if fmt.Sprint(r["name"]) == name {
				uri := strings.TrimLeft(fmt.Sprint(r["uri"]), "/")
				return textResult(base + "/" + uri), nil
			}
		}
		return toolResult{}, fmt.Errorf("named route %q not found", name)
	}

	path := strings.TrimLeft(argString(args, "path"), "/")
	return textResult(base + "/" + path), nil
}

// docsUserAgent mirrors laravel/boost's hosted-docs request so the API treats
// us identically.
const docsUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:140.0) Gecko/20100101 Firefox/140.0 Laravel Boost"

func docsSearch(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
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
	if len(cleaned) == 0 {
		return toolResult{}, errors.New("queries is required")
	}

	tokenLimit := argClampInt(args, "token_limit", 3000, 1000000)

	// Package context (name + major.x), like Boost's Roster, from composer.lock.
	type docPkg struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	var packages []docPkg
	if p, err := resolveProject(ctx, req); err == nil {
		if cl, err := p.composerLock(); err == nil {
			filter := map[string]bool{}
			for _, n := range argStrSlice(args, "packages") {
				filter[n] = true
			}
			for _, c := range append(cl.Packages, cl.PackagesDev...) {
				if len(filter) > 0 && !filter[c.Name] {
					continue
				}
				if mv := majorX(c.Version); mv != "" {
					packages = append(packages, docPkg{c.Name, mv})
				}
			}
		}
	}

	body, err := json.Marshal(map[string]any{
		"queries":     cleaned,
		"packages":    packages,
		"token_limit": tokenLimit,
		"format":      "markdown",
	})
	if err != nil {
		return toolResult{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.DocsURL+"/api/docs", bytes.NewReader(body))
	if err != nil {
		return toolResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", docsUserAgent)
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return toolResult{}, fmt.Errorf("docs search request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return toolResult{}, fmt.Errorf("docs search returned %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	return textResult(string(raw)), nil
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

// artisanAllow is the set of read-only artisan commands the `artisan` tool may
// run. Anything that mutates state is intentionally excluded — use `tinker`
// (opt-in) for that.
var artisanAllow = map[string]bool{
	"about": true, "env": true,
	"db:show": true, "db:table": true, "db:monitor": true,
	"migrate:status": true,
	"queue:failed":   true, "queue:monitor": true,
	"schedule:list": true, "schedule:test": true,
	"event:list": true, "route:list": true, "model:show": true,
	"channel:list": true, "config:show": true, "view:cache": false,
	"permission:show": true, "about:json": false,
}

func artisan(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	command := strings.TrimSpace(argString(args, "command"))
	if command == "" {
		return toolResult{}, errors.New("command is required")
	}
	if !artisanAllow[command] {
		allowed := make([]string, 0, len(artisanAllow))
		for c, ok := range artisanAllow {
			if ok {
				allowed = append(allowed, c)
			}
		}
		sort.Strings(allowed)
		return toolErrResult(fmt.Sprintf(
			"Refused: %q is not an allowed read-only artisan command. Allowed: %s. For arbitrary commands enable and use `tinker`.",
			command, strings.Join(allowed, ", "))), nil
	}
	p, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}
	cmdArgs := append([]string{command}, argStrSlice(args, "args")...)
	out, err := p.runArtisan(ctx, cmdArgs...)
	if err != nil {
		return toolResult{}, err
	}
	if strings.TrimSpace(out) == "" {
		return textResult("(no output)"), nil
	}
	return textResult(out), nil
}

func tinker(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	code := argString(args, "code")
	if code == "" {
		return toolResult{}, errors.New("code is required")
	}
	p, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}
	out, err := p.runArtisan(ctx, "tinker", "--execute="+code)
	if err != nil {
		return toolResult{}, err
	}
	if strings.TrimSpace(out) == "" {
		return textResult("(no output)"), nil
	}
	return textResult(out), nil
}
