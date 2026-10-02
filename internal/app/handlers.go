package app

import (
	"context"
	"os/exec"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// phpVersion returns the runtime PHP version (e.g. "8.3.10"), or "" if php is
// unavailable.
func phpVersion(ctx context.Context, root string) string {
	//nolint:gosec // G204: the php binary is the user's own LARAVEL_MCP_PHP setting; the script is a constant
	cmd := exec.CommandContext(ctx, cfg.PHPBin, "-r", "echo PHP_VERSION;")
	cmd.Dir = root

	out, err := cmd.Output()
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(out))
}

func appInfo(ctx context.Context, req *mcp.CallToolRequest) (toolResult, error) {
	proj, err := resolveProject(ctx, req)
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
		Root:           proj.Root,
		PHPVersion:     phpVersion(ctx, proj.Root),
		LaravelVersion: "", // from composer.lock below
		AppName:        proj.Env("APP_NAME", ""),
		AppEnv:         proj.Env("APP_ENV", ""),
		AppDebug:       proj.Env("APP_DEBUG", ""),
		Telescope:      proj.hasPackage("laravel/telescope"),
		Packages:       nil,
	}

	lock, lockErr := proj.composerLock()
	if lockErr == nil {
		for _, lockPkg := range lock.Packages {
			if lockPkg.Name == "laravel/framework" {
				info.LaravelVersion = lockPkg.Version
			}

			info.Packages = append(info.Packages, pkg(lockPkg))
		}

		for _, lockPkg := range lock.PackagesDev {
			info.Packages = append(info.Packages, pkg(lockPkg))
		}

		slices.SortFunc(info.Packages, func(a, b pkg) int { return strings.Compare(a.Name, b.Name) })
	}

	return jsonResult(ctx, info), nil
}
