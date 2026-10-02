package app

import (
	"context"
	"crypto/sha1" //nolint:gosec // hashes file-cache paths like Laravel's FileStore; not a security primitive
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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

// state tool kinds.
const (
	stateKindCache = "cache"
	stateKindQueue = keyQueue
)

const (
	// stateDefaultName is Laravel's stock name for the cache's redis
	// connection and database table alike.
	stateDefaultName = "cache"
	// stateKeyStore is the result key naming the inspected cache store.
	stateKeyStore = "store"
	// stateKeyFile is the result key naming a file-cache entry's path.
	stateKeyFile = "file"
	// stateCacheSampleKeys caps the key sample a redis cache overview lists.
	stateCacheSampleKeys = 50
	// stateCacheTableLimit caps the rows read from a database cache table.
	stateCacheTableLimit = 500
	// stateQueueSampleSize caps the pending and failed jobs a queue shows.
	stateQueueSampleSize = 10
	// fileCacheExpiryLen is the length of the expiry timestamp that prefixes
	// every FileStore entry.
	fileCacheExpiryLen = 10
)

var errStateUnsafeName = errors.New("unsafe")

func state(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	proj, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}

	proj.envName = argString(args, "env")

	switch strings.ToLower(argString(args, "kind")) {
	case stateKindCache:
		return stateCache(ctx, proj, args)
	case stateKindQueue:
		return stateQueue(ctx, proj, args)
	default:
		return toolErrResult("Refused: `kind` is required (cache or queue)."), nil
	}
}

// ── config helpers ───────────────────────────────────────────────────────────

func (p *Project) confStr(key, def string) string {
	val, ok, err := p.config(key)
	if err != nil || !ok || val == nil {
		return def
	}

	if s := fmt.Sprint(val); s != "" {
		return s
	}

	return def
}

func (p *Project) confMap(key string) map[string]any {
	val, ok, err := p.config(key)
	if err != nil || !ok {
		return nil
	}

	return asMap(val)
}

// resolveRedis returns the connection target for a named redis connection from
// config/database.php, falling back to the REDIS_* env vars.
func (p *Project) resolveRedis(connName string) redisTarget {
	redisCfg := p.confMap("database.redis." + connName)
	host := orDefault(asStr(redisCfg[dbKeyHost]), p.Env("REDIS_HOST", dbDefaultHost))
	port := orDefault(asStr(redisCfg[dbKeyPort]), p.Env("REDIS_PORT", "6379"))
	dbIndex, _ := strconv.Atoi(orDefault(asStr(redisCfg[keyDatabase]), p.Env("REDIS_DB", "0")))

	return redisTarget{
		addr:     net.JoinHostPort(host, port),
		username: orDefault(asStr(redisCfg[dbKeyUsername]), p.Env("REDIS_USERNAME", "")),
		password: orDefault(asStr(redisCfg[keyPassword]), p.Env("REDIS_PASSWORD", "")),
		db:       dbIndex,
	}
}

// ── cache ────────────────────────────────────────────────────────────────────

func stateCache(ctx context.Context, proj *Project, args map[string]any) (toolResult, error) {
	store := argString(args, "store")
	if store == "" {
		store = proj.confStr("cache.default", proj.Env("CACHE_STORE", proj.Env("CACHE_DRIVER", driverFile)))
	}

	scfg := proj.confMap("cache.stores." + store)

	driver := asStr(scfg[keyDriver])
	if driver == "" {
		driver = store
	}

	key := argString(args, "key")

	switch driver {
	case driverRedis:
		return cacheRedis(ctx, proj, store, scfg, key)
	case driverDatabase:
		return cacheDatabase(ctx, proj, store, scfg, key)
	case driverFile:
		return cacheFile(ctx, proj, store, scfg, key)
	default:
		return jsonResult(ctx, map[string]any{
			stateKeyStore: store, keyDriver: driver,
			keyNote: "live inspection supported for redis, database, file stores only",
		}), nil
	}
}

func cacheRedis(ctx context.Context, proj *Project, store string, scfg map[string]any, key string) (toolResult, error) {
	conn := orDefault(asStr(scfg[keyConnection]), stateDefaultName)
	target := proj.resolveRedis(conn)
	prefix := proj.confStr("database.redis.options.prefix", "") +
		proj.confStr("cache.prefix", proj.Env("CACHE_PREFIX", ""))

	client, err := dialRedis(ctx, target)
	if err != nil {
		return toolResult{}, err
	}
	defer func() { _ = client.Close() }()

	out := map[string]any{
		stateKeyStore: store, keyDriver: driverRedis, keyConnection: conn,
		"address": target.addr, "db": target.db, "key_prefix": prefix,
	}
	if key != "" {
		return jsonResult(ctx, redisCacheLookup(client, out, prefix, key)), nil
	}

	out["dbsize"], _ = client.intCmd("DBSIZE")

	keys, err := client.scan(prefix+"*", stateCacheSampleKeys)
	if err == nil {
		out["sample_keys"] = keys
	}

	return jsonResult(ctx, out), nil
}

// redisCacheLookup fills out with key's value, trying it with and without the
// cache prefix.
func redisCacheLookup(client *redisConn, out map[string]any, prefix, key string) map[string]any {
	for _, candidate := range []string{prefix + key, key} {
		val, found, err := client.getString(candidate)
		if err != nil || !found {
			continue
		}

		ttl, _ := client.intCmd("TTL", candidate)
		out["key"], out["value"], out["ttl_seconds"], out["found"] = candidate, decodeMaybe(val), ttl, true

		return out
	}

	out["found"], out["tried"] = false, []string{prefix + key, key}

	return out
}

func cacheDatabase(
	ctx context.Context,
	proj *Project,
	store string,
	scfg map[string]any,
	key string,
) (toolResult, error) {
	table := orDefault(asStr(scfg[keyTable]), stateDefaultName)

	res, _, err := proj.readTable(ctx, asStr(scfg[keyConnection]), table, nil, stateCacheTableLimit)
	if err != nil {
		return toolResult{}, err
	}

	rows := filterRowsByCol(res, "key", key)

	return jsonResult(ctx, map[string]any{
		stateKeyStore: store, keyDriver: driverDatabase, keyTable: table,
		"matched": len(rows), "entries": rows,
	}), nil
}

func cacheFile(ctx context.Context, proj *Project, store string, scfg map[string]any, key string) (toolResult, error) {
	dir := orDefault(asStr(scfg[keyPath]), proj.path("storage", "framework", "cache", "data"))

	out := map[string]any{stateKeyStore: store, keyDriver: driverFile, keyPath: dir}
	if key == "" {
		out["files"] = countFiles(dir)
		out[keyNote] = "pass a key to read its value"

		return jsonResult(ctx, out), nil
	}
	// FileStore path: sha1(prefix+key) chunked into /AA/BB/<hash>. The prefix the
	// store sees is version-dependent, so try with and without cache.prefix.
	prefix := proj.confStr("cache.prefix", proj.Env("CACHE_PREFIX", ""))
	for _, candidate := range []string{key, prefix + key} {
		path := fileCachePath(dir, candidate)

		raw, err := os.ReadFile(path) //nolint:gosec // G304: reading the app's own file-cache entry is the point
		if err != nil {
			continue
		}

		body := string(raw)
		if len(body) >= fileCacheExpiryLen {
			body = body[fileCacheExpiryLen:]
		}

		out["key"], out[stateKeyFile], out["value"], out["found"] = candidate, path, body, true
		out[keyNote] = "value is PHP-serialized (returned raw)"

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

func stateQueue(ctx context.Context, proj *Project, args map[string]any) (toolResult, error) {
	conn := argString(args, keyConnection)
	if conn == "" {
		conn = proj.confStr("queue.default", proj.Env("QUEUE_CONNECTION", driverSync))
	}

	qcfg := proj.confMap("queue.connections." + conn)

	driver := asStr(qcfg[keyDriver])
	if driver == "" {
		driver = conn
	}

	name := argString(args, keyQueue)
	if name == "" {
		name = orDefault(asStr(qcfg[keyQueue]), "default")
	}

	out := map[string]any{keyConnection: conn, keyDriver: driver, keyQueue: name}

	switch driver {
	case driverDatabase:
		queuePendingDatabase(ctx, proj, qcfg, name, out)
	case driverRedis:
		queuePendingRedis(ctx, proj, qcfg, name, out)
	case driverSync:
		out[keyNote] = "sync driver runs jobs inline; nothing is queued"
	default:
		// Other drivers (sqs, beanstalkd, …) have no live inspection; the
		// failed jobs below still apply.
	}

	// Failed jobs live in the database (failed_jobs) regardless of queue driver.
	failedConn := proj.confStr("queue.failed.database", "")

	res, total, err := proj.readTable(ctx, failedConn, "failed_jobs", nil, stateQueueSampleSize)
	if err == nil {
		out["failed"], out["failed_sample"] = total, decodePayloads(rowsToMaps(res), stateQueueSampleSize)
	}

	return jsonResult(ctx, out), nil
}

// queuePendingDatabase adds the pending jobs of a database queue to out.
func queuePendingDatabase(ctx context.Context, proj *Project, qcfg map[string]any, name string, out map[string]any) {
	table := orDefault(asStr(qcfg[keyTable]), "jobs")
	filter := &tableFilter{col: keyQueue, val: name}

	res, total, err := proj.readTable(ctx, asStr(qcfg[keyConnection]), table, filter, stateQueueSampleSize)
	if err != nil {
		out["pending_error"] = err.Error()

		return
	}

	out["pending"], out["pending_sample"] = total, decodePayloads(rowsToMaps(res), stateQueueSampleSize)
}

// queuePendingRedis adds the pending jobs of a redis queue to out.
func queuePendingRedis(ctx context.Context, proj *Project, qcfg map[string]any, name string, out map[string]any) {
	rconn := orDefault(asStr(qcfg[keyConnection]), "default")
	target := proj.resolveRedis(rconn)
	listKey := proj.confStr("database.redis.options.prefix", "") + "queues:" + name
	out["list_key"] = listKey

	client, err := dialRedis(ctx, target)
	if err != nil {
		out["pending_error"] = err.Error()

		return
	}
	defer func() { _ = client.Close() }()

	out["pending"], _ = client.intCmd("LLEN", listKey)

	items, err := client.lrange(listKey, stateQueueSampleSize)
	if err == nil {
		out["pending_sample"] = decodeStrings(items)
	}
}

// ── shared table reads / decoding ────────────────────────────────────────────

// tableFilter narrows readTable to the rows whose column equals a value.
type tableFilter struct{ col, val string }

// readTable returns up to limit rows of table, narrowed by filter when non-nil,
// along with how many rows match in total. Table and column names are
// validated against identRe, since they can't be bound as parameters.
func (p *Project) readTable(
	ctx context.Context,
	conn, table string,
	filter *tableFilter,
	limit int,
) (*queryResult, int, error) {
	if !identRe.MatchString(table) {
		return nil, 0, fmt.Errorf("%w table name %q", errStateUnsafeName, table)
	}

	if filter != nil && !identRe.MatchString(filter.col) {
		return nil, 0, fmt.Errorf("%w column name %q", errStateUnsafeName, filter.col)
	}

	sqlDB, err := p.openDBPinged(ctx, conn)
	if err != nil {
		return nil, 0, err
	}

	driver := sqlDB.driver

	defer func() { _ = sqlDB.Close() }()

	var (
		where string
		args  []any
	)
	if filter != nil {
		where, args = " WHERE "+quoteIdentFor(driver, filter.col)+" = ?", []any{filter.val}
	}

	var total int

	countQuery := rebind(driver, "SELECT COUNT(*) FROM "+table+where)

	err = sqlDB.QueryRowContext(ctx, countQuery, args...).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("count rows in %s: %w", table, err)
	}

	// Every column: these are Laravel's own tables, whose columns vary by version.
	//nolint:unqueryvet // table is identRe-validated; the full row is what the state tool shows
	rowsQuery := rebind(driver, "SELECT * FROM "+table+where+" LIMIT "+strconv.Itoa(limit))

	res, err := queryRows(ctx, sqlDB, rowsQuery, args...)
	if err != nil {
		return nil, 0, err
	}

	return res, total, nil
}

func rowsToMaps(res *queryResult) []map[string]any {
	out := make([]map[string]any, 0, len(res.Rows))
	for _, row := range res.Rows {
		rowMap := make(map[string]any, len(res.Columns))
		for i, c := range res.Columns {
			if i < len(row) {
				rowMap[c] = row[i]
			}
		}

		out = append(out, rowMap)
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

// decodeMaybe parses raw as JSON when it looks like JSON, else returns it as is.
func decodeMaybe(raw string) any {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		return raw
	}

	var parsed any

	err := json.Unmarshal([]byte(trimmed), &parsed)
	if err == nil {
		return parsed
	}

	return raw
}

func countFiles(dir string) int {
	count := 0
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			count++
		}

		return nil
	})

	return count
}
