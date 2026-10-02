package app

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// doctor.go runs cheap, local "why isn't this working" checks: missing
// .env/APP_KEY, .env keys absent vs .env.example, stale caches that silently
// ignore source edits, database connectivity, pending migrations, and
// known-vulnerable composer packages. Every check degrades to a clear
// skip/error rather than failing the whole call.

const (
	doctorCheckEnvFile     = "env_file"
	doctorCheckMigrations  = "migrations"
	doctorCheckAudit       = "composer_audit"
	doctorCacheTimeLayout  = "2006-01-02 15:04"
	doctorSlowChecksNumber = 3
)

type doctorCheck struct {
	Name   string `json:"check"`
	Status string `json:"status"` // ok | warn | error | skip
	Detail string `json:"detail,omitempty"`
}

func doctor(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	proj, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}

	checks := doctorEnvChecks(proj)
	checks = append(checks, doctorCacheChecks(proj)...)
	checks = append(checks, doctorSlowChecks(ctx, proj, args)...)

	return jsonResult(ctx, struct {
		Root   string        `json:"root"`
		Checks []doctorCheck `json:"checks"`
	}{proj.Root, checks}), nil
}

// doctorEnvChecks covers .env, its keys vs .env.example, APP_KEY and the
// environment summary.
func doctorEnvChecks(proj *Project) []doctorCheck {
	var checks []doctorCheck

	_, err := os.Stat(proj.path(".env"))
	if err != nil {
		checks = append(checks, doctorCheck{
			doctorCheckEnvFile, statusError, ".env missing — copy .env.example, then `php artisan key:generate`",
		})
	} else {
		checks = append(checks, doctorCheck{doctorCheckEnvFile, statusOK, ""})
		checks = append(checks, envKeysCheck(proj)...)
	}

	if proj.Env("APP_KEY", "") == "" {
		checks = append(checks, doctorCheck{"app_key", statusError, "APP_KEY empty — run `php artisan key:generate`"})
	} else {
		checks = append(checks, doctorCheck{"app_key", statusOK, ""})
	}

	return append(checks, doctorCheck{
		"environment", statusOK, "APP_ENV=" + proj.Env("APP_ENV", "?") + " APP_DEBUG=" + proj.Env("APP_DEBUG", "?"),
	})
}

// envKeysCheck flags keys present in .env.example but absent from .env — the
// classic "pulled a branch that added a config var" trap. No check when there
// is no .env.example to compare against.
func envKeysCheck(proj *Project) []doctorCheck {
	example := proj.envFile(".env.example")
	if len(example) == 0 {
		return nil
	}

	env := proj.envFile(".env")

	var missing []string

	for key := range example {
		if _, ok := env[key]; !ok {
			missing = append(missing, key)
		}
	}

	if len(missing) == 0 {
		return []doctorCheck{{"env_keys", statusOK, ""}}
	}

	slices.Sort(missing)

	return []doctorCheck{
		{"env_keys", statusWarn, "in .env.example but missing from .env: " + strings.Join(missing, ", ")},
	}
}

// doctorCacheChecks flags stale caches: a present cache file silently
// overrides later source/.env edits in dev — the classic "my change does
// nothing" trap.
func doctorCacheChecks(proj *Project) []doctorCheck {
	checks := []doctorCheck{
		cacheCheck("config", proj.path("bootstrap", "cache", "config.php"), "config:clear"),
		cacheCheck("event", proj.path("bootstrap", "cache", "events.php"), "event:clear"),
	}

	routes, _ := filepath.Glob(proj.path("bootstrap", "cache", "routes*.php"))
	if len(routes) == 0 {
		return append(checks, doctorCheck{"route_cache", statusOK, ""})
	}

	return append(checks, cacheCheck("route", routes[0], "route:clear"))
}

// doctorSlowChecks runs the checks that wait on the network or boot PHP
// concurrently; each writes only its own slot, keeping the report order.
func doctorSlowChecks(ctx context.Context, proj *Project, args map[string]any) []doctorCheck {
	runAudit := !has(args, "audit") || argBool(args, "audit")

	var (
		slow  [doctorSlowChecksNumber]doctorCheck
		group sync.WaitGroup
	)

	group.Go(func() { slow[0] = databaseCheck(ctx, proj) })
	group.Go(func() { slow[1] = migrationsCheck(ctx, proj) })
	group.Go(func() {
		if !runAudit {
			slow[2] = doctorCheck{doctorCheckAudit, statusSkip, "disabled (audit=false)"}

			return
		}

		slow[2] = auditCheck(ctx, proj)
	})
	group.Wait()

	return slow[:]
}

func databaseCheck(ctx context.Context, proj *Project) doctorCheck {
	dbConn, err := proj.openDB(ctx, "")
	if err != nil {
		return doctorCheck{keyDatabase, statusError, "could not open connection: " + err.Error()}
	}
	defer func() { _ = dbConn.Close() }()

	err = dbConn.PingContext(ctx)
	if err != nil {
		return doctorCheck{keyDatabase, statusError, "ping failed: " + err.Error()}
	}

	return doctorCheck{keyDatabase, statusOK, ""}
}

// migrationsCheck flags pending migrations: the actual DB schema lags what the
// migrations define — the classic "table/column doesn't exist" after pulling a
// branch.
func migrationsCheck(ctx context.Context, proj *Project) doctorCheck {
	out, err := proj.runArtisan(ctx, "migrate:status")
	if err != nil {
		return doctorCheck{
			doctorCheckMigrations, statusSkip, "could not run migrate:status (needs php + a reachable DB)",
		}
	}

	if pending := countPendingMigrations(out); pending > 0 {
		return doctorCheck{
			doctorCheckMigrations,
			statusWarn,
			fmt.Sprintf("%d pending migration(s) — run `php artisan migrate`", pending),
		}
	}

	return doctorCheck{doctorCheckMigrations, statusOK, ""}
}

// countPendingMigrations counts not-yet-run migrations in `migrate:status`
// output, handling both the modern "<name> ... Pending" format and the legacy
// "| No  | <name> |" table.
func countPendingMigrations(out string) int {
	pending := 0

	for line := range strings.SplitSeq(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasSuffix(trimmed, "Pending") || strings.HasPrefix(trimmed, "| No ") {
			pending++
		}
	}

	return pending
}

func cacheCheck(label, path, clearCmd string) doctorCheck {
	name := label + "_cache"

	info, err := os.Stat(path)
	if err != nil {
		return doctorCheck{name, statusOK, ""}
	}

	return doctorCheck{name, statusWarn, label + " is cached (" + filepath.Base(path) + ", " +
		info.ModTime().
			Format(doctorCacheTimeLayout) +
		") — source/.env edits ignored until `php artisan " + clearCmd + "`"}
}

// auditCheck runs `composer audit` against composer.lock and summarizes any
// advisories. composer exits non-zero when advisories exist, but still prints
// the JSON on stdout, so the exit code is ignored.
func auditCheck(ctx context.Context, proj *Project) doctorCheck {
	_, err := exec.LookPath("composer")
	if err != nil {
		return doctorCheck{doctorCheckAudit, statusSkip, "composer not on PATH"}
	}

	_, err = os.Stat(proj.path("composer.lock"))
	if err != nil {
		return doctorCheck{doctorCheckAudit, statusSkip, "no composer.lock"}
	}

	cmd := exec.CommandContext(ctx, "composer", "audit", "--locked", "--format=json", "--no-interaction")
	cmd.Dir = proj.Root
	out, _ := cmd.Output()

	names, ok := parseAuditAdvisories(out)
	if !ok {
		return doctorCheck{doctorCheckAudit, statusSkip, "could not parse composer audit output"}
	}

	if len(names) == 0 {
		return doctorCheck{doctorCheckAudit, statusOK, "no known vulnerabilities"}
	}

	return doctorCheck{
		doctorCheckAudit, statusWarn,
		strconv.Itoa(len(names)) + " vulnerable package(s): " + strings.Join(names, ", "),
	}
}

// parseAuditAdvisories extracts the advisory package names from `composer audit
// --format=json` output. Returns ok=false when the bytes aren't valid audit JSON.
func parseAuditAdvisories(out []byte) ([]string, bool) {
	var res struct {
		Advisories json.RawMessage `json:"advisories"`
	}

	err := json.Unmarshal(out, &res)
	if err != nil || res.Advisories == nil {
		return nil, false
	}

	// PHP encodes an empty array as [], so a clean audit is a list, not a map.
	var byPackage map[string]json.RawMessage

	err = json.Unmarshal(res.Advisories, &byPackage)
	if err != nil {
		var none []json.RawMessage
		if json.Unmarshal(res.Advisories, &none) == nil && len(none) == 0 {
			return []string{}, true
		}

		return nil, false
	}

	return slices.Sorted(maps.Keys(byPackage)), true
}
