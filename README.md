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
  directly (mysql / mariadb / pgsql / sqlite). Falls back to `php`/`php artisan`
  only where the live app is unavoidable: `routes`, `artisan`, `tinker`,
  named-route `absolute_url`, and `config`/`db_connections` when a config uses
  array spreads or other dynamic code. `php` must be on `PATH` (or set
  `LARAVEL_MCP_PHP`) for those. `doctor` additionally shells to `composer audit`
  when `composer` is present (skips cleanly otherwise). Everything else —
  including `telescope_prune` (a direct `DELETE`) — is pure Go.
- **Works without Telescope** — Telescope-backed tools degrade with a clear
  message; everything else keeps working.

## Install

npm (auto-updating — `@latest` always fetches the newest release binary):

```sh
npx -y @stubbedev/laravel-dev-mcp@latest
```

Or pin it in an MCP client config:

```json
{
  "mcpServers": {
    "laravel-dev": {
      "command": "npx",
      "args": ["-y", "@stubbedev/laravel-dev-mcp@latest"]
    }
  }
}
```

The npm package is a thin launcher that downloads the prebuilt Go binary for
your platform (linux/macOS, x64/arm64) on install — or on first run if the
install-time download is skipped. Set `LARAVEL_MCP_SKIP_DOWNLOAD=1` to skip it.

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
| `LARAVEL_MCP_HTTP` | — | Truthy (`1`/`true`) enables HTTP on the default `127.0.0.1:8765`. |
| `LARAVEL_MCP_HTTP_PATH` | `/mcp` | HTTP endpoint path (`--http-path`). |
| `LARAVEL_MCP_TOKEN` | — | If set, HTTP clients must send it as `Authorization: Bearer <token>` (or `X-Mcp-Token`). Ignored for stdio. |
| `LARAVEL_MCP_DOCS_URL` | `https://boost.laravel.com` | Docs search backend. |

## Tools

The tools are **gated**: only `laravel_debug` is exposed until it is called.
Calling it reveals the rest for that session (per-session, so unrelated sessions
stay uncluttered), and an MCP client that supports `tools/list_changed` picks
them up automatically.

- `laravel_debug` — reveal the tools below for the current session.

Works on any Laravel app:

- `app_info` — PHP/Laravel versions, env, installed composer packages.
- `doctor` — health check: missing `.env`/`APP_KEY`, stale config/route/event caches (which silently ignore source & `.env` edits), DB connectivity, vulnerable composer packages (`composer audit`). Set `audit=false` to skip the network call.
- `profile` — per-request profiling: total time, query breakdown, slowest queries, and **N+1 detection** (statements that ran 2+ times after normalizing bound values). Reads from **Telescope** (`telescope_entries`, grouped by request — no extra package needed), or the **Clockwork** (`storage/clockwork`) / **Debugbar** (`storage/debugbar`) storage files when present. Auto-detects the source; no app boot.
- `state` — live backend state. `kind=cache` reads a cache entry (value + TTL) or, without a `key`, store stats/sample keys; `kind=queue` reports pending + recent failed jobs with decoded payloads. Backends: database, **redis** (built-in client — no dependency), file. Honors `env=testing`.
- `db_connections` — every configured connection (from `config/database.php`, passwords masked).
- `db_schema` — list tables, or describe one table's columns/indexes/FKs (honors table prefix).
- `db_query` — **read-only** SQL (SELECT/SHOW/EXPLAIN/DESCRIBE/WITH/PRAGMA only).
  - The three `db_*` tools accept `env=testing` (or any env name) to target the **test database**: it overlays `.env.testing` and, for `testing`, `phpunit.xml` `<env>` entries over `.env` — matching how Laravel resolves config under tests.
- `models` — discover Eloquent models across `app/`, `app/Modules/`, `Modules/`, `src/` (AST parse, no PHP run): table, fillable, casts, relationships.
- `logs` — application + frontend logs. `source=app` (default) tails the newest `laravel*.log` (resolved via `config/logging.php`, honoring a custom `LOG_CHANNEL`/path and daily rotation), filterable by `level`; `source=error` returns the last error-level entry; `source=browser` tails `storage/logs/browser.log`.
- `routes` — `php artisan route:list`, filterable.
- `config` — read any `config/*.php` value by dotted key (native AST parse, env-resolved, **secrets redacted**); falls back to PHP when a config uses array spreads / dynamic code so nothing is omitted; omit the key to list config files.
- `absolute_url` — build a URL from `APP_URL` (path or named route).
- `artisan` — run allowlisted **read-only** artisan commands (`about`, `db:show`, `migrate:status`, `queue:failed`, `schedule:list`, `event:list`, …).
- `docs_search` — search version-matched Laravel-ecosystem docs.
- `tinker` — execute arbitrary PHP (opt-in via `LARAVEL_MCP_TINKER=1`).

Requires Laravel Telescope (degrades gracefully when absent):

- `telescope` — query telemetry by type (`requests`, `queries`, `exceptions`,
  `jobs`, `mail`, `cache`, …), with filters `slow`, `since_hours`, `tag`,
  `request_id`, and `id` (single entry).
- `telescope_prune` — delete old Telescope entries (`hours`, default 24).

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
