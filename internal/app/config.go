package app

import (
	"os"
	"strings"
)

// Config holds the resolved server-wide settings. Per-repo settings (DB creds,
// paths) are resolved per request from the Laravel project root, not here.
type Config struct {
	// PHPBin is the php executable used to shell out to artisan. Default "php".
	PHPBin string
	// DocsURL is the base URL for the docs_search proxy.
	DocsURL string
	// AuthToken, when set, requires HTTP clients to present it as a bearer token
	// (Authorization: Bearer <token>) or X-Mcp-Token header. Ignored for stdio.
	AuthToken string
}

func loadConfig() Config {
	cfg := Config{
		PHPBin:    envOr("LARAVEL_MCP_PHP", "php"),
		DocsURL:   envOr("LARAVEL_MCP_DOCS_URL", "https://boost.laravel.com"),
		AuthToken: os.Getenv("LARAVEL_MCP_TOKEN"),
	}
	return cfg
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
