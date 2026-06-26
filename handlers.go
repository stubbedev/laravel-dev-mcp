package main

import (
	"context"
	"os/exec"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// phpVersion returns the runtime PHP version (e.g. "8.3.10"), or "" if php is
// unavailable.
func phpVersion(ctx context.Context, root string) string {
	cmd := exec.CommandContext(ctx, cfg.PHPBin, "-r", "echo PHP_VERSION;")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func appInfo(ctx context.Context, req *mcp.CallToolRequest) (toolResult, error) {
	p, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}

	type pkg struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	info := struct {
		Root           string `json:"root"`
		PHPVersion     string `json:"php_version,omitempty"`
		LaravelVersion string `json:"laravel_version,omitempty"`
		AppName        string `json:"app_name,omitempty"`
		AppEnv         string `json:"app_env,omitempty"`
		AppDebug       string `json:"app_debug,omitempty"`
		Telescope      bool   `json:"telescope_installed"`
		Packages       []pkg  `json:"packages"`
	}{
		Root:      p.Root,
		AppName:   p.Env("APP_NAME", ""),
		AppEnv:    p.Env("APP_ENV", ""),
		AppDebug:  p.Env("APP_DEBUG", ""),
		Telescope: p.hasPackage("laravel/telescope"),
	}

	info.PHPVersion = phpVersion(ctx, p.Root)

	if cl, err := p.composerLock(); err == nil {
		for _, c := range cl.Packages {
			if c.Name == "laravel/framework" {
				info.LaravelVersion = c.Version
			}
			info.Packages = append(info.Packages, pkg(c))
		}
		for _, c := range cl.PackagesDev {
			info.Packages = append(info.Packages, pkg(c))
		}
		sort.Slice(info.Packages, func(i, j int) bool { return info.Packages[i].Name < info.Packages[j].Name })
	}

	return jsonResult(ctx, info), nil
}
