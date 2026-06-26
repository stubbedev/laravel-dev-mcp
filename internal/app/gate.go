package app

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stubbedev/laravel-dev-mcp/version"
)

// gateToolName is the single always-visible tool. Until it is called, every
// Laravel tool stays hidden so the server adds nothing to an unrelated
// session's context. See toolServer for why this is per-session.
const gateToolName = "laravel_debug"

type toolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

var (
	parseOnce  sync.Once
	gateDef    toolDef
	gatedTools []toolDef // every tool hidden until activation
)

// parseTools loads tools.json once: it builds the input-schema validators for
// every tool (validators are internal, so populating them costs no client
// context) and splits the gate tool from the gated set.
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
			if d.Name == gateToolName {
				gateDef = d
				continue
			}
			gatedTools = append(gatedTools, d)
		}
	})
}

// toolServer wraps one mcp.Server whose Laravel tools stay hidden until the
// laravel_debug gate is called. Each session gets its own toolServer — over
// stdio that is the per-client process; over HTTP the SDK calls the server
// factory once per new session — so activation is per-session and never
// pollutes another conversation's tool list.
type toolServer struct {
	srv       *mcp.Server
	activated atomic.Bool
}

func newToolServer() *toolServer {
	parseTools()
	ts := &toolServer{}
	ts.srv = mcp.NewServer(
		&mcp.Implementation{Name: "laravel-dev-mcp", Version: version.Version},
		&mcp.ServerOptions{Instructions: buildInstructions()},
	)
	if gateDef.Name != "" {
		ts.srv.AddTool(toolFromDef(gateDef), ts.onCall)
	}
	return ts
}

func toolFromDef(d toolDef) *mcp.Tool {
	return &mcp.Tool{Name: d.Name, Description: d.Description, InputSchema: d.InputSchema}
}

// activate exposes the full tool set on this session's server (idempotent). The
// SDK emits notifications/tools/list_changed so the client refreshes its list.
func (ts *toolServer) activate() {
	if ts.activated.Swap(true) {
		return
	}
	for _, d := range gatedTools {
		// tinker runs arbitrary PHP; only expose it when explicitly enabled.
		if d.Name == "tinker" && !cfg.TinkerEnabled {
			continue
		}
		ts.srv.AddTool(toolFromDef(d), ts.onCall)
	}
}

func (ts *toolServer) onCall(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if req.Params.Name == gateToolName {
		ts.activate()
		return toCallResult(textResult(gateActivationText())), nil
	}
	return dispatchCall(ctx, req)
}

// gateActivationText lists the tools the gate just unlocked, so the model can
// proceed even before its client processes the list_changed refresh.
func gateActivationText() string {
	var b strings.Builder
	b.WriteString("Laravel dev tools unlocked for this session:\n")
	for _, d := range gatedTools {
		if d.Name == "tinker" && !cfg.TinkerEnabled {
			continue
		}
		b.WriteString("- ")
		b.WriteString(d.Name)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}
