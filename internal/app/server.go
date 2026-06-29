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

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stubbedev/laravel-dev-mcp/version"
)

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[laravel-dev-mcp] "+format+"\n", args...)
}

// cfg is the process-wide configuration, set once at startup.
var cfg Config

// Run starts the MCP server (stdio or HTTP) and returns a process exit code.
func Run() int {
	opt, err := parseCLI(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		logf("invalid arguments: %v", err)
		return 2
	}
	if opt.version {
		fmt.Println("laravel-dev-mcp " + version.Version)
		return 0
	}

	cfg = loadConfig()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if opt.httpOn {
		// A fresh gated server per session: the SDK calls this factory only for
		// new sessions, so each session has its own tool visibility and one
		// session's laravel_debug activation never leaks into another.
		getServer := func(*http.Request) *mcp.Server { return newToolServer().srv }
		if err := serveHTTP(ctx, getServer, opt.httpAddr, opt.httpPath); err != nil {
			logf("http server error: %v", err)
			return 1
		}
		return 0
	}

	ts := newToolServer()
	if err := ts.srv.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		logf("stdio server error: %v", err)
		return 1
	}
	return 0
}

// validators holds the resolved input schema per tool name, populated by
// parseTools. Validation is internal, so it covers every tool regardless of
// whether the tool is currently exposed to the client.
var validators = map[string]*jsonschema.Resolved{}

// toolCallTimeout caps total wall-clock for one tool call (a php boot can be
// slow on a cold app).
const toolCallTimeout = 60 * time.Second

var errUnknownTool = errors.New("unknown tool")

// dispatchCall validates, formats, and routes one tools/call. The gate
// (laravel_debug) is intercepted earlier in toolServer.onCall.
func dispatchCall(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, toolCallTimeout)
	defer cancel()

	rawArgs := map[string]any{}
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &rawArgs); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
	}
	if v := validators[req.Params.Name]; v != nil {
		if err := v.Validate(rawArgs); err != nil {
			return toolErr("Invalid arguments: " + err.Error()), nil
		}
	}

	ctx = ctxWithFormat(ctx, pickFormat(rawArgs))

	result, err := runTool(ctx, req, req.Params.Name, rawArgs)
	if err != nil {
		if errors.Is(err, errUnknownTool) {
			return nil, fmt.Errorf("unknown tool: %s", req.Params.Name)
		}
		return toolErr("Error: " + err.Error()), nil
	}
	return toCallResult(capResult(result)), nil
}

func toolErr(msg string) *mcp.CallToolResult {
	return toCallResult(toolResult{Content: []contentBlock{{Type: "text", Text: msg}}, IsError: true})
}

func toCallResult(r toolResult) *mcp.CallToolResult {
	out := &mcp.CallToolResult{IsError: r.IsError}
	for _, c := range r.Content {
		out.Content = append(out.Content, &mcp.TextContent{Text: c.Text})
	}
	return out
}

func pickFormat(args map[string]any) string {
	if f := strings.ToLower(argString(args, "format")); f == "json" || f == "toon" {
		return f
	}
	return "toon"
}

// runTool dispatches every tools/call to its handler. Each handler resolves the
// Laravel project from the request roots itself.
func runTool(ctx context.Context, req *mcp.CallToolRequest, name string, args map[string]any) (toolResult, error) {
	switch name {
	case "app_info":
		return appInfo(ctx, req)
	case "db_connections":
		return dbConnections(ctx, req, args)
	case "db_schema":
		return dbSchema(ctx, req, args)
	case "db_query":
		return dbQuery(ctx, req, args)
	case "logs":
		return logs(ctx, req, args)
	case "routes":
		return routes(ctx, req, args)
	case "config":
		return configValue(ctx, req, args)
	case "absolute_url":
		return absoluteURL(ctx, req, args)
	case "docs_search":
		return docsSearch(ctx, req, args)
	case "tinker":
		return tinker(ctx, req, args)
	case "artisan":
		return artisan(ctx, req, args)
	case "models":
		return models(ctx, req, args)
	case "doctor":
		return doctor(ctx, req, args)
	case "profile":
		return profile(ctx, req, args)
	case "state":
		return state(ctx, req, args)
	case "telescope":
		return telescope(ctx, req, args)
	case "telescope_prune":
		return telescopePrune(ctx, req, args)
	default:
		return toolResult{}, errUnknownTool
	}
}

func buildInstructions() string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s); b.WriteByte('\n') }

	w("# laravel-dev-mcp")
	w("")
	w(
		"Local Laravel development tooling. Inspect a Laravel app's packages, database, routes, config, logs, and (when installed) Telescope telemetry. Prefer these tools over shelling out to `php artisan`, `mysql`, or reading files by hand.",
	)
	w("")
	w("## Project root")
	w(
		"- Every tool operates on one Laravel app, resolved from the MCP workspace root (or the `X-Mcp-Root` HTTP header), falling back to the working directory. Over HTTP, set `X-Mcp-Root` per request to target different repos/worktrees from a single server.",
	)
	w("")
	w("## Activation")
	w(
		"- The inspection tools are hidden until `laravel_debug` is called once. Call it yourself — without being asked, and without the user naming any tool — the first time the work touches a Laravel app here (an error/500 to chase, logs, database, routes, config/.env, models, jobs, Telescope, or a general 'is my setup OK' check). The tools then appear for the rest of the session; unrelated sessions stay uncluttered.",
	)
	w("")
	w("## Tools")
	w("- `laravel_debug` — unlock the tools below for this session (call first).")
	w("- `doctor` — health-check: missing .env/APP_KEY, stale config/route/event caches, DB connectivity, vulnerable composer packages.")
	w("- `profile` — per-request profiling from Clockwork/Debugbar: timing, query breakdown, slowest queries, N+1 detection.")
	w("- `state` — live cache entries and queue/failed jobs (`kind=cache|queue`; database / redis / file backends).")
	w("- `app_info` — PHP/Laravel versions and installed composer packages.")
	w("- `db_connections` / `db_schema` / `db_query` — inspect the database directly (read-only SQL).")
	w("- `models` — discover Eloquent models (table, fillable, casts, relationships) across app/, Modules/, src/.")
	w("- `logs` — application + frontend logs (`source=app|error|browser`).")
	w("- `routes` / `config` / `absolute_url` — route, config (secrets redacted), and URL introspection.")
	w("- `artisan` — run allowlisted read-only artisan commands (about, db:show, migrate:status, queue:failed, …).")
	w("- `docs_search` — search version-matched Laravel-ecosystem docs.")
	w(
		"- `telescope` — query Telescope telemetry (requests, queries, exceptions, jobs, …) when Telescope is installed; degrades cleanly when it is not. `telescope_prune` clears old entries.",
	)
	w("- `tinker` — execute arbitrary PHP in the app context when laravel/tinker is installed; degrades cleanly when it is not.")
	w("")
	w("## Notes")
	w("- `db_query` is read-only (SELECT/SHOW/EXPLAIN/DESCRIBE only).")
	w("- For historical \"what ran\" data (past queries, exceptions, jobs) use `telescope`; for live state use `db_query`/`db_schema`.")
	return b.String()
}
