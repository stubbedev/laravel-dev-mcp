package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func main() { os.Exit(run()) }

func run() int {
	for _, a := range os.Args[1:] {
		if a == "--version" || a == "-v" || a == "version" {
			fmt.Println("laravel-dev-mcp " + version.Version)
			return 0
		}
	}

	cfg = loadConfig()

	srv := mcp.NewServer(
		&mcp.Implementation{Name: "laravel-dev-mcp", Version: version.Version},
		&mcp.ServerOptions{Instructions: buildInstructions()},
	)
	registerTools(srv)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if addr := httpAddr(); addr != "" {
		if err := serveHTTP(ctx, srv, addr, httpPath()); err != nil {
			logf("http server error: %v", err)
			return 1
		}
		return 0
	}

	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		logf("stdio server error: %v", err)
		return 1
	}
	return 0
}

// validators holds the resolved input schema per tool name.
var validators = map[string]*jsonschema.Resolved{}

func registerTools(srv *mcp.Server) {
	var tools []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	if err := json.Unmarshal([]byte(toolsJSON), &tools); err != nil {
		logf("tool schema parse error: %v", err)
		return
	}
	for _, t := range tools {
		// tinker runs arbitrary PHP; only expose it when explicitly enabled.
		if t.Name == "tinker" && !cfg.TinkerEnabled {
			continue
		}
		srv.AddTool(&mcp.Tool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema}, toolHandler)

		var schema jsonschema.Schema
		if err := json.Unmarshal(t.InputSchema, &schema); err != nil {
			logf("tool %s: schema parse error: %v", t.Name, err)
			continue
		}
		resolved, err := schema.Resolve(nil)
		if err != nil {
			logf("tool %s: schema resolve error: %v", t.Name, err)
			continue
		}
		validators[t.Name] = resolved
	}
}

// toolCallTimeout caps total wall-clock for one tool call (a php boot can be
// slow on a cold app).
const toolCallTimeout = 60 * time.Second

var errUnknownTool = errors.New("unknown tool")

func toolHandler(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		return dbConnections(ctx, req)
	case "db_schema":
		return dbSchema(ctx, req, args)
	case "db_query":
		return dbQuery(ctx, req, args)
	case "read_logs":
		return readLogs(ctx, req, args)
	case "last_error":
		return lastError(ctx, req)
	case "browser_logs":
		return browserLogs(ctx, req, args)
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
	w("## Tools")
	w("- `app_info` — PHP/Laravel versions and installed composer packages.")
	w("- `db_connections` / `db_schema` / `db_query` — inspect the database directly (read-only SQL).")
	w("- `models` — discover Eloquent models (table, fillable, casts, relationships) across app/, Modules/, src/.")
	w("- `read_logs` / `last_error` / `browser_logs` — application + frontend logs.")
	w("- `routes` / `config` / `absolute_url` — route, config (secrets redacted), and URL introspection.")
	w("- `artisan` — run allowlisted read-only artisan commands (about, db:show, migrate:status, queue:failed, …).")
	w("- `docs_search` — search version-matched Laravel-ecosystem docs.")
	w(
		"- `telescope` — query Telescope telemetry (requests, queries, exceptions, jobs, …) when Telescope is installed; degrades cleanly when it is not. `telescope_prune` clears old entries.",
	)
	if cfg.TinkerEnabled {
		w("- `tinker` — execute arbitrary PHP in the app context (enabled).")
	}
	w("")
	w("## Notes")
	w("- `db_query` is read-only (SELECT/SHOW/EXPLAIN/DESCRIBE only).")
	w("- For historical \"what ran\" data (past queries, exceptions, jobs) use `telescope`; for live state use `db_query`/`db_schema`.")
	return b.String()
}
