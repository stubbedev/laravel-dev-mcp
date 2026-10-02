// Package version holds the build version, overridden via -ldflags at release
// time (see package.nix and .github/workflows/release.yml).
package version

// Version is the server version. Defaults to "dev"; release builds set it via
// -X github.com/stubbedev/laravel-dev-mcp/version.Version=<tag>.
var Version = "dev" //nolint:gochecknoglobals // -ldflags -X can only set a package-level string var
