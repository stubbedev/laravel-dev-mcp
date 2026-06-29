package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// doctor.go runs cheap, local "why isn't this working" checks: missing
// .env/APP_KEY, .env keys absent vs .env.example, stale caches that silently
// ignore source edits, database connectivity, pending migrations, and
// known-vulnerable composer packages. Every check degrades to a clear
// skip/error rather than failing the whole call.

type doctorCheck struct {
	Name   string `json:"check"`
	Status string `json:"status"` // ok | warn | error | skip
	Detail string `json:"detail,omitempty"`
}

func doctor(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	p, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}

	var checks []doctorCheck
	add := func(name, status, detail string) {
		checks = append(checks, doctorCheck{name, status, detail})
	}

	if _, err := os.Stat(p.path(".env")); err != nil {
		add("env_file", "error", ".env missing — copy .env.example, then `php artisan key:generate`")
	} else {
		add("env_file", "ok", "")

		// Keys present in .env.example but absent from .env — the classic
		// "pulled a branch that added a config var" trap.
		if example := p.envFile(".env.example"); len(example) > 0 {
			env := p.envFile(".env")
			var missing []string
			for k := range example {
				if _, ok := env[k]; !ok {
					missing = append(missing, k)
				}
			}
			if len(missing) > 0 {
				sort.Strings(missing)
				add("env_keys", "warn", "in .env.example but missing from .env: "+strings.Join(missing, ", "))
			} else {
				add("env_keys", "ok", "")
			}
		}
	}

	if p.Env("APP_KEY", "") == "" {
		add("app_key", "error", "APP_KEY empty — run `php artisan key:generate`")
	} else {
		add("app_key", "ok", "")
	}

	add("environment", "ok", "APP_ENV="+p.Env("APP_ENV", "?")+" APP_DEBUG="+p.Env("APP_DEBUG", "?"))

	// Stale caches: a present cache file silently overrides later source/.env
	// edits in dev — the classic "my change does nothing" trap.
	addCache(add, "config", p.path("bootstrap", "cache", "config.php"), "config:clear")
	addCache(add, "event", p.path("bootstrap", "cache", "events.php"), "event:clear")
	if m, _ := filepath.Glob(p.path("bootstrap", "cache", "routes*.php")); len(m) > 0 {
		addCache(add, "route", m[0], "route:clear")
	} else {
		add("route_cache", "ok", "")
	}

	if db, _, _, err := p.openDB(ctx, ""); err != nil {
		add("database", "error", "could not open connection: "+err.Error())
	} else {
		func() {
			defer func() { _ = db.Close() }()
			if err := db.PingContext(ctx); err != nil {
				add("database", "error", "ping failed: "+err.Error())
			} else {
				add("database", "ok", "")
			}
		}()
	}

	// Pending migrations: the actual DB schema lags what the migrations define —
	// the classic "table/column doesn't exist" after pulling a branch.
	if out, err := p.runArtisan(ctx, "migrate:status"); err != nil {
		add("migrations", "skip", "could not run migrate:status (needs php + a reachable DB)")
	} else if n := countPendingMigrations(out); n > 0 {
		add("migrations", "warn", fmt.Sprintf("%d pending migration(s) — run `php artisan migrate`", n))
	} else {
		add("migrations", "ok", "")
	}

	runAudit := true
	if has(args, "audit") {
		runAudit = argBool(args, "audit")
	}
	if !runAudit {
		add("composer_audit", "skip", "disabled (audit=false)")
	} else {
		st, detail := auditStatus(ctx, p)
		add("composer_audit", st, detail)
	}

	return jsonResult(ctx, struct {
		Root   string        `json:"root"`
		Checks []doctorCheck `json:"checks"`
	}{p.Root, checks}), nil
}

// countPendingMigrations counts not-yet-run migrations in `migrate:status`
// output, handling both the modern "<name> ... Pending" format and the legacy
// "| No  | <name> |" table.
func countPendingMigrations(out string) int {
	n := 0
	for line := range strings.SplitSeq(out, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasSuffix(l, "Pending") || strings.HasPrefix(l, "| No ") {
			n++
		}
	}
	return n
}

func addCache(add func(string, string, string), label, path, clearCmd string) {
	if fi, err := os.Stat(path); err == nil {
		add(label+"_cache", "warn", label+" is cached ("+filepath.Base(path)+", "+
			fi.ModTime().Format("2006-01-02 15:04")+") — source/.env edits ignored until `php artisan "+clearCmd+"`")
	} else {
		add(label+"_cache", "ok", "")
	}
}

// auditStatus runs `composer audit` against composer.lock and summarizes any
// advisories. composer exits non-zero when advisories exist, but still prints
// the JSON on stdout, so the exit code is ignored.
func auditStatus(ctx context.Context, p *Project) (string, string) {
	if _, err := exec.LookPath("composer"); err != nil {
		return "skip", "composer not on PATH"
	}
	if _, err := os.Stat(p.path("composer.lock")); err != nil {
		return "skip", "no composer.lock"
	}
	cmd := exec.CommandContext(ctx, "composer", "audit", "--locked", "--format=json", "--no-interaction")
	cmd.Dir = p.Root
	out, _ := cmd.Output()
	names, ok := parseAuditAdvisories(out)
	if !ok {
		return "skip", "could not parse composer audit output"
	}
	if len(names) == 0 {
		return "ok", "no known vulnerabilities"
	}
	return "warn", strconv.Itoa(len(names)) + " vulnerable package(s): " + strings.Join(names, ", ")
}

// parseAuditAdvisories extracts the advisory package names from `composer audit
// --format=json` output. Returns ok=false when the bytes aren't valid audit JSON.
func parseAuditAdvisories(out []byte) ([]string, bool) {
	var res struct {
		Advisories map[string]json.RawMessage `json:"advisories"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, false
	}
	names := make([]string, 0, len(res.Advisories))
	for k := range res.Advisories {
		names = append(names, k)
	}
	sort.Strings(names)
	return names, true
}
