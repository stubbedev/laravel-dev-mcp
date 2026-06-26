package app

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var errorLevels = map[string]bool{"ERROR": true, "CRITICAL": true, "ALERT": true, "EMERGENCY": true}

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
	m, _ := cfgv.(map[string]any)
	if m == nil {
		return nil
	}
	def, _ := m["default"].(string)
	channels, _ := m["channels"].(map[string]any)

	var globs []string
	for _, path := range channelLogPaths(channels, def, map[string]bool{}) {
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
	ch, _ := channels[name].(map[string]any)
	if ch == nil {
		return nil
	}
	switch driver, _ := ch["driver"].(string); driver {
	case "single", "daily":
		if path, ok := ch["path"].(string); ok && path != "" {
			return []string{path}
		}
	case "stack":
		var out []string
		if members, ok := ch["channels"].([]any); ok {
			for _, mem := range members {
				if sub, ok := mem.(string); ok {
					out = append(out, channelLogPaths(channels, sub, seen)...)
				}
			}
		}
		return out
	}
	return nil
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
	newest, newestMod := "", int64(0)
	for _, g := range globs {
		matches, _ := filepath.Glob(g)
		for _, m := range matches {
			fi, err := os.Stat(m)
			if err != nil {
				continue
			}
			if mod := fi.ModTime().UnixNano(); newest == "" || mod >= newestMod {
				newest, newestMod = m, mod
			}
		}
	}
	return newest
}

// logs reads application or frontend logs. source=app (default) tails the newest
// laravel*.log; source=error returns the last error-level entry from it;
// source=browser tails storage/logs/browser.log.
func logs(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	p, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}

	source := strings.ToLower(argString(args, "source"))
	if source == "" {
		source = "app"
	}

	if source == "browser" {
		return browserLog(p, args)
	}
	if source != "app" && source != "error" {
		return toolErrResult("Refused: unknown source " + source + " (use app, error, or browser)."), nil
	}

	path := p.appLogPath()
	if path == "" {
		return textResult("No application log file found (checked config/logging.php and storage/logs/laravel*.log)."), nil
	}
	raw, err := tailBytes(path, maxLogTail)
	if err != nil {
		return toolResult{}, err
	}
	entries := parseLogEntries(string(raw))

	if source == "error" {
		for _, e := range slices.Backward(entries) {
			if errorLevels[e.Level] {
				return jsonResult(ctx, e), nil
			}
		}
		return textResult("No error-level entries found in the log tail."), nil
	}

	if lvl := strings.ToUpper(argString(args, "level")); lvl != "" {
		filtered := entries[:0]
		for _, e := range entries {
			if e.Level == lvl {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}
	entries = lastN(entries, argClampInt(args, "limit", 50, 200))
	if len(entries) == 0 {
		return textResult("No matching log entries."), nil
	}
	return jsonResult(ctx, entries), nil
}

func browserLog(p *Project, args map[string]any) (toolResult, error) {
	path := p.path("storage", "logs", "browser.log")
	raw, err := tailBytes(path, maxLogTail)
	if err != nil {
		if os.IsNotExist(err) {
			return textResult("No browser log at storage/logs/browser.log. Frontend logging is not set up."), nil
		}
		return toolResult{}, err
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	n := argClampInt(args, "limit", 50, 200)
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		return textResult("Browser log is empty."), nil
	}
	return textResult(strings.Join(lines, "\n")), nil
}
