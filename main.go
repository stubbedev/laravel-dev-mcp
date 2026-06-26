// Command laravel-dev-mcp is a low-footprint MCP server for local Laravel
// development. All logic lives in internal/app; this is just the entrypoint.
package main

import (
	"os"

	"github.com/stubbedev/laravel-dev-mcp/internal/app"
)

func main() { os.Exit(app.Run()) }
