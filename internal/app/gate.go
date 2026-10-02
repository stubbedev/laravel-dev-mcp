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
	InputSchema json.RawMessage `json:"inputSchema"` //nolint:tagliatelle // MCP wire-format name, as tools.json spells it
}

// toolRegistry is the parsed tool set: the definitions exposed to clients, the
// resolved input schema per tool name, and the handler per tool name.
// Validation is internal, so it covers every tool regardless of whether the
// tool is currently exposed to the client.
type toolRegistry struct {
	tools      []toolDef
	validators map[string]*jsonschema.Resolved
	handlers   map[string]toolHandler
}

// loadTools parses tools.json once, building the input-schema validators for
// every tool (validators are internal, so populating them costs no client
// context).
//
//nolint:gochecknoglobals // the tool registry is process-wide, built once from the embedded tools.json
var loadTools = sync.OnceValue(buildRegistry)

func buildRegistry() *toolRegistry {
	reg := &toolRegistry{
		tools:      nil,
		validators: map[string]*jsonschema.Resolved{},
		handlers:   toolHandlers(),
	}

	var defs []toolDef

	err := json.Unmarshal([]byte(toolsJSON), &defs)
	if err != nil {
		logf("tool schema parse error: %v", err)

		return reg
	}

	for _, def := range defs {
		var schema jsonschema.Schema

		err := json.Unmarshal(def.InputSchema, &schema)
		if err != nil {
			logf("tool %s: schema parse error: %v", def.Name, err)

			continue
		}

		resolved, err := schema.Resolve(nil)
		if err != nil {
			logf("tool %s: schema resolve error: %v", def.Name, err)

			continue
		}

		reg.validators[def.Name] = resolved
		reg.tools = append(reg.tools, def)
	}

	return reg
}

// newServer builds one session's mcp.Server with the full Laravel tool set
// exposed up front.
func newServer() *mcp.Server {
	// The SDK defaults are what we want for everything but the instructions.
	opts := new(mcp.ServerOptions)
	opts.Instructions = serverInstructions

	srv := mcp.NewServer(
		&mcp.Implementation{
			Name:        "laravel-dev-mcp",
			Title:       "",
			Description: "",
			Version:     version.Version,
			WebsiteURL:  "",
			Icons:       nil,
		},
		opts,
	)
	for _, def := range loadTools().tools {
		srv.AddTool(toolFromDef(def), dispatchCall)
	}

	return srv
}

func toolFromDef(def toolDef) *mcp.Tool {
	return &mcp.Tool{
		Meta:         nil,
		Annotations:  nil,
		Description:  def.Description,
		InputSchema:  def.InputSchema,
		Name:         def.Name,
		OutputSchema: nil,
		Title:        "",
		Icons:        nil,
	}
}
