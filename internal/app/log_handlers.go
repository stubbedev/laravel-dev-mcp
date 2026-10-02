package app

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	logDefaultLimit = 50
	logMaxLimit     = 200
)

// isErrorLevel reports whether a Monolog level is error-or-worse.
func isErrorLevel(level string) bool {
	switch level {
	case "ERROR", "CRITICAL", "ALERT", "EMERGENCY":
		return true
	default:
		return false
	}
}

// appLogPath returns the newest file the app actually logs to. It reads
// config/logging.php to honor a custom LOG_CHANNEL / path (including a `stack`
// that fans out to single/daily channels); if that can't be resolved it falls
// back to the default storage/logs/laravel*.log glob.
func (p *Project) appLogPath() string {
	if globs := p.configuredLogGlobs(); len(globs) > 0 {
		if path := newestMatch(globs); path != "" {
			return path
		}
	}

	return newestMatch([]string{p.path("storage", "logs", "laravel*.log")})
}

// configuredLogGlobs resolves the default channel from config/logging.php into
// the glob(s) of files it writes. Returns nil when logging isn't file-based or
// the config can't be read statically.
func (p *Project) configuredLogGlobs() []string {
	cfgv, ok, err := p.config("logging")
	if err != nil || !ok {
		return nil
	}

	logging := asMap(cfgv)
	if logging == nil {
		return nil
	}

	channels := asMap(logging["channels"])

	var globs []string
	for _, path := range channelLogPaths(channels, asStr(logging["default"]), map[string]bool{}) {
		// single → the exact file; daily → laravel.log rotated to laravel-DATE.log.
		globs = append(globs, path, dailyGlob(path))
	}

	return globs
}

// channelLogPaths collects the file paths a channel writes to, expanding a
// `stack` driver into its member channels. seen guards against cycles.
func channelLogPaths(channels map[string]any, name string, seen map[string]bool) []string {
	if name == "" || seen[name] {
		return nil
	}

	seen[name] = true

	channel := asMap(channels[name])
	if channel == nil {
		return nil
	}

	switch asStr(channel[keyDriver]) {
	case driverSingle, driverDaily:
		if path, ok := channel[keyPath].(string); ok && path != "" {
			return []string{path}
		}

		return nil
	case driverStack:
		return stackChannelPaths(channels, channel, seen)
	default:
		return nil
	}
}

// stackChannelPaths expands a `stack` channel's members into their paths.
func stackChannelPaths(channels, stack map[string]any, seen map[string]bool) []string {
	var out []string

	for _, member := range asArr(stack["channels"]) {
		if sub, ok := member.(string); ok {
			out = append(out, channelLogPaths(channels, sub, seen)...)
		}
	}

	return out
}

// dailyGlob turns "/logs/laravel.log" into "/logs/laravel-*.log" to match the
// date-suffixed files the daily driver writes.
func dailyGlob(path string) string {
	ext := filepath.Ext(path)

	return strings.TrimSuffix(path, ext) + "-*" + ext
}

// newestMatch returns the most recently modified file across the given globs,
// or "" when none exist.
func newestMatch(globs []string) string {
	newest, newestNano := "", int64(0)

	for _, glob := range globs {
		matches, _ := filepath.Glob(glob)
		for _, match := range matches {
			info, err := os.Stat(match)
			if err != nil {
				continue
			}

			if modNano := info.ModTime().UnixNano(); newest == "" || modNano >= newestNano {
				newest, newestNano = match, modNano
			}
		}
	}

	return newest
}

// logs reads application or frontend logs. source=app (default) tails the newest
// laravel*.log; source=error returns the last error-level entry from it;
// source=browser tails storage/logs/browser.log.
func logs(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	proj, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}

	source := strings.ToLower(argString(args, "source"))
	if source == "" {
		source = sourceApp
	}

	switch source {
	case sourceBrowser:
		return browserLog(proj, args)
	case sourceApp, sourceError:
		return appLog(ctx, proj, source, args)
	default:
		return toolErrResult("Refused: unknown source " + source + " (use app, error, or browser)."), nil
	}
}

// appLog serves source=app (filtered tail) and source=error (last error-level
// entry) from the newest application log.
func appLog(ctx context.Context, proj *Project, source string, args map[string]any) (toolResult, error) {
	path := proj.appLogPath()
	if path == "" {
		return textResult(
			"No application log file found (checked config/logging.php and storage/logs/laravel*.log).",
		), nil
	}

	raw, err := tailBytes(path, maxLogTail)
	if err != nil {
		return toolResult{}, err
	}

	entries := parseLogEntries(string(raw))

	if source == sourceError {
		return lastErrorEntry(ctx, entries), nil
	}

	entries = filterLogLevel(entries, strings.ToUpper(argString(args, "level")))

	entries = lastN(entries, argClampInt(args, "limit", logDefaultLimit, logMaxLimit))
	if len(entries) == 0 {
		return textResult("No matching log entries."), nil
	}

	return jsonResult(ctx, entries), nil
}

func lastErrorEntry(ctx context.Context, entries []logEntry) toolResult {
	for _, entry := range slices.Backward(entries) {
		if isErrorLevel(entry.Level) {
			return jsonResult(ctx, entry)
		}
	}

	return textResult("No error-level entries found in the log tail.")
}

// filterLogLevel keeps only entries at level; an empty level keeps all.
func filterLogLevel(entries []logEntry, level string) []logEntry {
	if level == "" {
		return entries
	}

	filtered := entries[:0]
	for _, entry := range entries {
		if entry.Level == level {
			filtered = append(filtered, entry)
		}
	}

	return filtered
}

func browserLog(proj *Project, args map[string]any) (toolResult, error) {
	path := proj.path("storage", "logs", "browser.log")

	raw, err := tailBytes(path, maxLogTail)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return textResult("No browser log at storage/logs/browser.log. Frontend logging is not set up."), nil
		}

		return toolResult{}, err
	}

	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")

	limit := argClampInt(args, "limit", logDefaultLimit, logMaxLimit)
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}

	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		return textResult("Browser log is empty."), nil
	}

	return textResult(strings.Join(lines, "\n")), nil
}
