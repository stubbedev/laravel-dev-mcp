package app

import (
	"encoding/json"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stubbedev/laravel-dev-mcp/version"
)

type toolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

var (
	parseOnce sync.Once
	allTools  []toolDef
)

// parseTools loads tools.json once, building the input-schema validators for
// every tool (validators are internal, so populating them costs no client
// context).
func parseTools() {
	parseOnce.Do(func() {
		var defs []toolDef
		if err := json.Unmarshal([]byte(toolsJSON), &defs); err != nil {
			logf("tool schema parse error: %v", err)
			return
		}
		for _, d := range defs {
			var schema jsonschema.Schema
			if err := json.Unmarshal(d.InputSchema, &schema); err != nil {
				logf("tool %s: schema parse error: %v", d.Name, err)
				continue
			}
			resolved, err := schema.Resolve(nil)
			if err != nil {
				logf("tool %s: schema resolve error: %v", d.Name, err)
				continue
			}
			validators[d.Name] = resolved
			allTools = append(allTools, d)
		}
	})
}

// newServer builds one session's mcp.Server with the full Laravel tool set
// exposed up front.
func newServer() *mcp.Server {
	parseTools()
	srv := mcp.NewServer(
		&mcp.Implementation{Name: "laravel-dev-mcp", Version: version.Version},
		&mcp.ServerOptions{Instructions: buildInstructions()},
	)
	for _, d := range allTools {
		srv.AddTool(toolFromDef(d), dispatchCall)
	}
	return srv
}

func toolFromDef(d toolDef) *mcp.Tool {
	return &mcp.Tool{Name: d.Name, Description: d.Description, InputSchema: d.InputSchema}
}
