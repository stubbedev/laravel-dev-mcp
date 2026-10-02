package app

// Database and backend driver names, as Laravel's config files spell them
// (after normalizeDriver for databases).
const (
	driverMySQL    = "mysql"
	driverPgSQL    = "pgsql"
	driverSQLite   = "sqlite"
	driverRedis    = "redis"
	driverDatabase = "database"
	driverFile     = "file"
	driverSync     = "sync"
	driverStack    = "stack"
	driverSingle   = "single"
	driverDaily    = "daily"
)

// Output formats a tool call may ask for with `format`.
const (
	formatJSON = "json"
	formatTOON = "toon"
)

// MCP content block type for plain text.
const contentText = "text"

// doctor check outcomes.
const (
	statusOK    = "ok"
	statusWarn  = "warn"
	statusError = "error"
	statusSkip  = "skip"
)

// Profiling and log sources.
const (
	sourceClockwork = "clockwork"
	sourceDebugbar  = "debugbar"
	sourceTelescope = "telescope"
	sourceApp       = "app"
	sourceError     = "error"
	sourceBrowser   = "browser"
)

// Keys shared by tool results and the config maps they are built from.
const (
	keyNote       = "note"
	keyDriver     = "driver"
	keyDatabase   = "database"
	keyPassword   = "password"
	keyPath       = "path"
	keyConnection = "connection"
	keyQueue      = "queue"
	keyTable      = "table"
	keyPrefix     = "prefix"
)
