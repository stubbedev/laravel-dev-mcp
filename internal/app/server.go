// Package app implements laravel-dev-mcp: an MCP server exposing a local
// Laravel app's packages, database, routes, config, logs, and Telescope
// telemetry as tools.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stubbedev/laravel-dev-mcp/version"
)

// Process exit codes returned by Run.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[laravel-dev-mcp] "+format+"\n", args...)
}

// cfg is the process-wide configuration, set once at startup.
//
//nolint:gochecknoglobals // process-wide settings read by every handler; set once in Run before serving
var cfg Config

// Run starts the MCP server (stdio or HTTP) and returns a process exit code.
func Run() int {
	opt, err := parseCLI(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}

		logf("%v", err)

		return exitUsage
	}

	if opt.version {
		_, _ = fmt.Fprintln(os.Stdout, "laravel-dev-mcp "+version.Version)

		return exitOK
	}

	cfg = loadConfig()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if opt.httpOn {
		// A fresh server per session: the SDK calls this factory only for new
		// sessions.
		getServer := func(*http.Request) *mcp.Server { return newServer() }

		err = serveHTTP(ctx, getServer, opt.httpAddr, opt.httpPath)
		if err != nil {
			logf("%v", err)

			return exitFailure
		}

		return exitOK
	}

	err = newServer().Run(ctx, &mcp.StdioTransport{MaxLineLength: 0})
	if err != nil && ctx.Err() == nil {
		logf("stdio server error: %v", err)

		return exitFailure
	}

	return exitOK
}

// toolCallTimeout caps total wall-clock for one tool call (a php boot can be
// slow on a cold app).
const toolCallTimeout = 60 * time.Second

var errUnknownTool = errors.New("unknown tool")

// dispatchCall validates, formats, and routes one tools/call.
func dispatchCall(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, toolCallTimeout)
	defer cancel()

	rawArgs := map[string]any{}
	if len(req.Params.Arguments) > 0 {
		err := json.Unmarshal(req.Params.Arguments, &rawArgs)
		if err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
	}

	if validator := loadTools().validators[req.Params.Name]; validator != nil {
		err := validator.Validate(rawArgs)
		if err != nil {
			return toolErr("Invalid arguments: " + err.Error()), nil
		}
	}

	ctx = ctxWithFormat(ctx, pickFormat(rawArgs))

	result, err := runTool(ctx, req, req.Params.Name, rawArgs)
	if err != nil {
		if errors.Is(err, errUnknownTool) {
			return nil, fmt.Errorf("%w: %s", errUnknownTool, req.Params.Name)
		}

		return toolErr("Error: " + err.Error()), nil
	}

	return toCallResult(capResult(result)), nil
}

func toolErr(msg string) *mcp.CallToolResult {
	return toCallResult(toolResult{Content: []contentBlock{{Type: contentText, Text: msg}}, IsError: true})
}

func toCallResult(result toolResult) *mcp.CallToolResult {
	out := &mcp.CallToolResult{
		Meta:              nil,
		Content:           nil,
		StructuredContent: nil,
		IsError:           result.IsError,
		InputRequests:     nil,
		RequestState:      "",
	}
	for _, block := range result.Content {
		out.Content = append(out.Content, &mcp.TextContent{Text: block.Text, Meta: nil, Annotations: nil})
	}

	return out
}

func pickFormat(args map[string]any) string {
	format := strings.ToLower(argString(args, "format"))
	if format == formatJSON || format == formatTOON {
		return format
	}

	return formatTOON
}

// Tool names that also appear elsewhere in the package as plain words.
const (
	toolConfig = "config"
	toolModels = "models"
)

// toolHandler serves one tool. Each handler resolves the Laravel project from
// the request roots itself.
type toolHandler func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error)

// toolHandlers maps every tool name in tools.json to its handler.
func toolHandlers() map[string]toolHandler {
	return map[string]toolHandler{
		"app_info": func(ctx context.Context, req *mcp.CallToolRequest, _ map[string]any) (toolResult, error) {
			return appInfo(ctx, req)
		},
		"db_connections":  dbConnections,
		"db_schema":       dbSchema,
		"db_query":        dbQuery,
		"logs":            logs,
		"routes":          routes,
		toolConfig:        configValue,
		"absolute_url":    absoluteURL,
		"docs_search":     docsSearch,
		"tinker":          tinker,
		"artisan":         artisan,
		toolModels:        models,
		"doctor":          doctor,
		"profile":         profile,
		"state":           state,
		sourceTelescope:   telescope,
		"telescope_prune": telescopePrune,
	}
}

// runTool dispatches one tools/call to its handler.
func runTool(ctx context.Context, req *mcp.CallToolRequest, name string, args map[string]any) (toolResult, error) {
	handler, ok := loadTools().handlers[name]
	if !ok {
		return toolResult{}, errUnknownTool
	}

	return handler(ctx, req, args)
}

// serverInstructions is the MCP initialize instructions text: one line per
// entry, each newline-terminated.
const serverInstructions = "# laravel-dev-mcp\n" +
	"\n" +
	"Local Laravel development tooling. Inspect a Laravel app's packages, database, routes, config, logs, " +
	"and (when installed) Telescope telemetry. Prefer these tools over shelling out to `php artisan`, " +
	"`mysql`, or reading files by hand.\n" +
	"\n" +
	"## Project root\n" +
	"- Every tool operates on one Laravel app, resolved from the MCP workspace root (or the `X-Mcp-Root` " +
	"HTTP header), falling back to the working directory. Over HTTP, set `X-Mcp-Root` per request to " +
	"target different repos/worktrees from a single server.\n" +
	"\n" +
	"## Tools\n" +
	"- `doctor` — health-check: missing .env/APP_KEY, stale config/route/event caches, DB connectivity, " +
	"vulnerable composer packages.\n" +
	"- `profile` — per-request profiling from Clockwork/Debugbar: timing, query breakdown, slowest queries, " +
	"N+1 detection.\n" +
	"- `state` — live cache entries and queue/failed jobs (`kind=cache|queue`; database / redis / file " +
	"backends).\n" +
	"- `app_info` — PHP/Laravel versions and installed composer packages.\n" +
	"- `db_connections` / `db_schema` / `db_query` — inspect the database directly (read-only SQL).\n" +
	"- `models` — discover Eloquent models (table, fillable, casts, relationships) across app/, Modules/, " +
	"src/.\n" +
	"- `logs` — application + frontend logs (`source=app|error|browser`).\n" +
	"- `routes` / `config` / `absolute_url` — route, config (secrets redacted), and URL introspection.\n" +
	"- `artisan` — run allowlisted read-only artisan commands (about, db:show, migrate:status, " +
	"queue:failed, …).\n" +
	"- `docs_search` — search version-matched Laravel-ecosystem docs.\n" +
	"- `telescope` — query Telescope telemetry (requests, queries, exceptions, jobs, …) when Telescope is " +
	"installed; degrades cleanly when it is not. `telescope_prune` clears old entries.\n" +
	"- `tinker` — execute arbitrary PHP in the app context when laravel/tinker is installed; degrades " +
	"cleanly when it is not.\n" +
	"\n" +
	"## Notes\n" +
	"- `db_query` is read-only (SELECT/SHOW/EXPLAIN/DESCRIBE only).\n" +
	"- For historical \"what ran\" data (past queries, exceptions, jobs) use `telescope`; for live state " +
	"use `db_query`/`db_schema`.\n"
