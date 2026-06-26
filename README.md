# laravel-dev-mcp

A fast, low-footprint [MCP](https://modelcontextprotocol.io) server for local
Laravel development, written in Go. It consolidates and replaces
[laravel/boost](https://github.com/laravel/boost) and
[laravel-telescope-mcp](https://github.com/lucianotonet/laravel-telescope-mcp)
into one binary — no PHP package to install in your app.

- **stdio and HTTP** transports.
- **MCP roots** (incl. the `X-Mcp-Root` HTTP header) so a single instance can
  serve many repos/worktrees.
- **Go-native** where possible: reads `.env`, `composer.lock`, `config/*.php`
  (parsed via a PHP AST — no PHP execution), log files, and talks to the database
  directly (mysql / mariadb / pgsql / sqlite). Shells out to `php artisan` only
  where the live app is unavoidable (routes, tinker, telescope:prune).
- **Works without Telescope** — Telescope-backed tools degrade with a clear
  message; everything else keeps working.

## Install

Homebrew:

```sh
brew install stubbedev/laravel-dev-mcp/laravel-dev-mcp
```

Nix (binary cache at `nix.stubbe.dev`):

```sh
nix run github:stubbedev/laravel-dev-mcp
```

Go:

```sh
go install github.com/stubbedev/laravel-dev-mcp@latest
```

## Usage

stdio (run from the Laravel app directory, or set the MCP root):

```sh
laravel-dev-mcp
```

HTTP (multi-repo; pass the project per request via header):

```sh
laravel-dev-mcp --http            # 127.0.0.1:8765/mcp
# then call /mcp with header:  X-Mcp-Root: /path/to/laravel-app
```

Example MCP client config (stdio):

```json
{
  "mcpServers": {
    "laravel-dev": {
      "command": "laravel-dev-mcp"
    }
  }
}
```

## Configuration

| Env var | Default | Purpose |
|---|---|---|
| `LARAVEL_MCP_PHP` | `php` | PHP binary used for artisan shell-outs. |
| `LARAVEL_MCP_TINKER` | unset | Set to `1` to expose the `tinker` tool (arbitrary PHP execution — off by default). |
| `LARAVEL_MCP_HTTP_ADDR` | — | Enable HTTP on this address (or use `--http`). |
| `LARAVEL_MCP_HTTP_PATH` | `/mcp` | HTTP endpoint path. |
| `LARAVEL_MCP_TOKEN` | — | If set, HTTP clients must send it as `Authorization: Bearer <token>` (or `X-Mcp-Token`). Ignored for stdio. |
| `LARAVEL_MCP_DOCS_URL` | `https://boost.laravel.com` | Docs search backend. |

## Tools

Works on any Laravel app:

- `app_info` — PHP/Laravel versions, env, installed composer packages.
- `db_connections` — every configured connection (from `config/database.php`, passwords masked).
- `db_schema` — list tables, or describe one table's columns/indexes/FKs (honors table prefix).
- `db_query` — **read-only** SQL (SELECT/SHOW/EXPLAIN/DESCRIBE/WITH/PRAGMA only).
- `models` — discover Eloquent models across `app/`, `app/Modules/`, `Modules/`, `src/` (AST parse, no PHP run): table, fillable, casts, relationships.
- `read_logs` / `last_error` — application log (`storage/logs/laravel.log`).
- `browser_logs` — frontend log (`storage/logs/browser.log`), if present.
- `routes` — `php artisan route:list`, filterable.
- `config` — read any `config/*.php` value by dotted key (native AST parse, env-resolved, **secrets redacted**); falls back to PHP when a config uses array spreads / dynamic code so nothing is omitted; omit the key to list config files.
- `absolute_url` — build a URL from `APP_URL` (path or named route).
- `artisan` — run allowlisted **read-only** artisan commands (`about`, `db:show`, `migrate:status`, `queue:failed`, `schedule:list`, `event:list`, …).
- `docs_search` — search version-matched Laravel-ecosystem docs.
- `tinker` — execute arbitrary PHP (opt-in via `LARAVEL_MCP_TINKER=1`).

Requires Laravel Telescope (degrades gracefully when absent):

- `telescope` — query telemetry by type (`requests`, `queries`, `exceptions`,
  `jobs`, `mail`, `cache`, …), with filters like `slow`, `request_id`, `id`.
- `telescope_prune` — delete old Telescope entries.

## Development

```sh
just build    # compile
just test     # go test ./...
just lint     # format + vet + golangci-lint
just check    # everything CI runs
```

Releases are cut with `just release-patch|minor|major` (tags `vX.Y.Z`), which
trigger multi-arch binary builds, the nix cache push, the GitHub release, and
the Homebrew tap bump.

## License

MIT
