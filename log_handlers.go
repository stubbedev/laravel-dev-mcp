package main

import (
	"context"
	"os"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var errorLevels = map[string]bool{"ERROR": true, "CRITICAL": true, "ALERT": true, "EMERGENCY": true}

func readLogs(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	p, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}
	path := p.path("storage", "logs", "laravel.log")
	raw, err := tailBytes(path, maxLogTail)
	if err != nil {
		if os.IsNotExist(err) {
			return textResult("No log file at storage/logs/laravel.log."), nil
		}
		return toolResult{}, err
	}
	entries := parseLogEntries(string(raw))

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

func lastError(ctx context.Context, req *mcp.CallToolRequest) (toolResult, error) {
	p, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}
	path := p.path("storage", "logs", "laravel.log")
	raw, err := tailBytes(path, maxLogTail)
	if err != nil {
		if os.IsNotExist(err) {
			return textResult("No log file at storage/logs/laravel.log."), nil
		}
		return toolResult{}, err
	}
	entries := parseLogEntries(string(raw))
	for _, e := range slices.Backward(entries) {
		if errorLevels[e.Level] {
			return jsonResult(ctx, e), nil
		}
	}
	return textResult("No error-level entries found in the log tail."), nil
}

func browserLogs(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	p, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}
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
