package app

import (
	"context"
	"crypto/sha1" //nolint:gosec // hashes file-cache paths like Laravel's FileStore; not a security primitive
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func sha1Hex(s string) string {
	h := sha1.Sum([]byte(s)) //nolint:gosec // file-cache path hashing, not security
	return hex.EncodeToString(h[:])
}

// state.go inspects live backend state: cache entries and queue/failed jobs,
// across the database, redis (via the built-in RESP client), and file backends.
// Connection/driver settings come from config/cache.php, config/queue.php and
// config/database.php (env-resolved), so it honors the `env` arg too — e.g.
// env=testing to look at the test database's queue.

var identRe = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

func state(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	p, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}
	p.envName = argString(args, "env")

	switch strings.ToLower(argString(args, "kind")) {
	case "cache":
		return stateCache(ctx, p, args)
	case "queue":
		return stateQueue(ctx, p, args)
	default:
		return toolErrResult("Refused: `kind` is required (cache or queue)."), nil
	}
}

// ── config helpers ───────────────────────────────────────────────────────────

func (p *Project) confStr(key, def string) string {
	if v, ok, err := p.config(key); err == nil && ok && v != nil {
		if s := fmt.Sprint(v); s != "" {
			return s
		}
	}
	return def
}

func (p *Project) confMap(key string) map[string]any {
	if v, ok, err := p.config(key); err == nil && ok {
		return asMap(v)
	}
	return nil
}

// resolveRedis returns the address/password/db for a named redis connection from
// config/database.php, falling back to the REDIS_* env vars.
func (p *Project) resolveRedis(connName string) (addr, password string, db int) {
	rc := p.confMap("database.redis." + connName)
	host := orDefault(asStr(rc["host"]), p.Env("REDIS_HOST", "127.0.0.1"))
	port := orDefault(asStr(rc["port"]), p.Env("REDIS_PORT", "6379"))
	password = orDefault(asStr(rc["password"]), p.Env("REDIS_PASSWORD", ""))
	db, _ = strconv.Atoi(orDefault(asStr(rc["database"]), p.Env("REDIS_DB", "0")))
	return host + ":" + port, password, db
}

// ── cache ────────────────────────────────────────────────────────────────────

func stateCache(ctx context.Context, p *Project, args map[string]any) (toolResult, error) {
	store := argString(args, "store")
	if store == "" {
		store = p.confStr("cache.default", p.Env("CACHE_STORE", p.Env("CACHE_DRIVER", "file")))
	}
	scfg := p.confMap("cache.stores." + store)
	driver := asStr(scfg["driver"])
	if driver == "" {
		driver = store
	}
	key := argString(args, "key")

	switch driver {
	case "redis":
		return cacheRedis(ctx, p, store, scfg, key)
	case "database":
		return cacheDatabase(ctx, p, store, scfg, key)
	case "file":
		return cacheFile(ctx, p, store, scfg, key)
	default:
		return jsonResult(ctx, map[string]any{
			"store": store, "driver": driver,
			"note": "live inspection supported for redis, database, file stores only",
		}), nil
	}
}

func cacheRedis(ctx context.Context, p *Project, store string, scfg map[string]any, key string) (toolResult, error) {
	conn := orDefault(asStr(scfg["connection"]), "cache")
	addr, pw, db := p.resolveRedis(conn)
	prefix := p.confStr("database.redis.options.prefix", "") + p.confStr("cache.prefix", p.Env("CACHE_PREFIX", ""))

	rc, err := dialRedis(ctx, addr, pw, db)
	if err != nil {
		return toolResult{}, fmt.Errorf("redis connect (%s): %w", addr, err)
	}
	defer func() { _ = rc.Close() }()

	out := map[string]any{"store": store, "driver": "redis", "connection": conn, "address": addr, "db": db, "key_prefix": prefix}
	if key != "" {
		for _, k := range []string{prefix + key, key} {
			if v, ok, err := rc.getString(k); err == nil && ok {
				ttl, _ := rc.intCmd("TTL", k)
				out["key"], out["value"], out["ttl_seconds"], out["found"] = k, decodeMaybe(v), ttl, true
				return jsonResult(ctx, out), nil
			}
		}
		out["found"], out["tried"] = false, []string{prefix + key, key}
		return jsonResult(ctx, out), nil
	}
	out["dbsize"], _ = rc.intCmd("DBSIZE")
	if keys, err := rc.scan(prefix+"*", 50); err == nil {
		out["sample_keys"] = keys
	}
	return jsonResult(ctx, out), nil
}

func cacheDatabase(ctx context.Context, p *Project, store string, scfg map[string]any, key string) (toolResult, error) {
	table := orDefault(asStr(scfg["table"]), "cache")
	res, err := p.readTable(ctx, asStr(scfg["connection"]), table, 500)
	if err != nil {
		return toolResult{}, err
	}
	rows := filterRowsByCol(res, "key", key)
	return jsonResult(ctx, map[string]any{
		"store": store, "driver": "database", "table": table,
		"matched": len(rows), "entries": rows,
	}), nil
}

func cacheFile(ctx context.Context, p *Project, store string, scfg map[string]any, key string) (toolResult, error) {
	dir := orDefault(asStr(scfg["path"]), p.path("storage", "framework", "cache", "data"))
	out := map[string]any{"store": store, "driver": "file", "path": dir}
	if key == "" {
		out["files"] = countFiles(dir)
		out["note"] = "pass a key to read its value"
		return jsonResult(ctx, out), nil
	}
	// FileStore path: sha1(prefix+key) chunked into /AA/BB/<hash>. The prefix the
	// store sees is version-dependent, so try with and without cache.prefix.
	prefix := p.confStr("cache.prefix", p.Env("CACHE_PREFIX", ""))
	for _, hk := range []string{key, prefix + key} {
		path := fileCachePath(dir, hk)
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		body := string(b)
		if len(body) >= 10 { // first 10 bytes are the expiry timestamp
			body = body[10:]
		}
		out["key"], out["file"], out["value"], out["found"] = hk, path, body, true
		out["note"] = "value is PHP-serialized (returned raw)"
		return jsonResult(ctx, out), nil
	}
	out["found"] = false
	return jsonResult(ctx, out), nil
}

func fileCachePath(dir, key string) string {
	h := sha1Hex(key)
	return filepath.Join(dir, h[0:2], h[2:4], h)
}

// ── queue ────────────────────────────────────────────────────────────────────

func stateQueue(ctx context.Context, p *Project, args map[string]any) (toolResult, error) {
	conn := argString(args, "connection")
	if conn == "" {
		conn = p.confStr("queue.default", p.Env("QUEUE_CONNECTION", "sync"))
	}
	qcfg := p.confMap("queue.connections." + conn)
	driver := asStr(qcfg["driver"])
	if driver == "" {
		driver = conn
	}
	name := argString(args, "queue")
	if name == "" {
		name = orDefault(asStr(qcfg["queue"]), "default")
	}

	out := map[string]any{"connection": conn, "driver": driver, "queue": name}

	switch driver {
	case "database":
		table := orDefault(asStr(qcfg["table"]), "jobs")
		if res, err := p.readTable(ctx, asStr(qcfg["connection"]), table, 200); err == nil {
			rows := filterRowsByCol(res, "queue", name)
			out["pending"], out["pending_sample"] = len(rows), decodePayloads(rows, 10)
		} else {
			out["pending_error"] = err.Error()
		}
	case "redis":
		rconn := orDefault(asStr(qcfg["connection"]), "default")
		addr, pw, db := p.resolveRedis(rconn)
		listKey := p.confStr("database.redis.options.prefix", "") + "queues:" + name
		out["list_key"] = listKey
		if rc, err := dialRedis(ctx, addr, pw, db); err == nil {
			defer func() { _ = rc.Close() }()
			out["pending"], _ = rc.intCmd("LLEN", listKey)
			if items, err := rc.lrange(listKey, 10); err == nil {
				out["pending_sample"] = decodeStrings(items)
			}
		} else {
			out["pending_error"] = fmt.Sprintf("redis connect (%s): %v", addr, err)
		}
	case "sync":
		out["note"] = "sync driver runs jobs inline; nothing is queued"
	}

	// Failed jobs live in the database (failed_jobs) regardless of queue driver.
	if res, err := p.readTable(ctx, p.confStr("queue.failed.database", ""), "failed_jobs", 10); err == nil {
		out["failed"], out["failed_sample"] = len(res.Rows), decodePayloads(rowsToMaps(res), 10)
	}
	return jsonResult(ctx, out), nil
}

// ── shared table reads / decoding ────────────────────────────────────────────

func (p *Project) readTable(ctx context.Context, conn, table string, limit int) (*queryResult, error) {
	if !identRe.MatchString(table) {
		return nil, fmt.Errorf("unsafe table name %q", table)
	}
	db, _, _, err := p.openDB(ctx, conn)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("could not connect to database: %w", err)
	}
	//nolint:unqueryvet // generic table inspector: columns vary per table, name is validated by identRe
	return queryRows(ctx, db, "SELECT * FROM "+table+" LIMIT "+strconv.Itoa(limit))
}

func rowsToMaps(res *queryResult) []map[string]any {
	out := make([]map[string]any, 0, len(res.Rows))
	for _, row := range res.Rows {
		m := make(map[string]any, len(res.Columns))
		for i, c := range res.Columns {
			if i < len(row) {
				m[c] = row[i]
			}
		}
		out = append(out, m)
	}
	return out
}

// filterRowsByCol returns the rows (as maps) whose named column contains needle
// (substring match; all rows when needle is empty).
func filterRowsByCol(res *queryResult, col, needle string) []map[string]any {
	var out []map[string]any
	for _, m := range rowsToMaps(res) {
		if needle == "" || strings.Contains(fmt.Sprint(m[col]), needle) {
			out = append(out, m)
		}
	}
	return out
}

// decodePayloads turns each row's JSON `payload` column into a nested object for
// readability, capping the count.
func decodePayloads(rows []map[string]any, limit int) []map[string]any {
	if len(rows) > limit {
		rows = rows[:limit]
	}
	for _, m := range rows {
		if pv, ok := m["payload"]; ok {
			m["payload"] = decodeMaybe(fmt.Sprint(pv))
		}
	}
	return rows
}

func decodeStrings(items []string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = decodeMaybe(s)
	}
	return out
}

// decodeMaybe parses s as JSON when it looks like JSON, else returns it raw.
func decodeMaybe(s string) any {
	t := strings.TrimSpace(s)
	if t == "" || (t[0] != '{' && t[0] != '[') {
		return s
	}
	var v any
	if err := json.Unmarshal([]byte(t), &v); err == nil {
		return v
	}
	return s
}

func countFiles(dir string) int {
	n := 0
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}
