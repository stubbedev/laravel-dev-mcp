package app

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpRoot is a workspace root exposed by the MCP client (the "roots" feature):
// the repo/working-tree the server should operate in.
type mcpRoot struct {
	URI  string
	Name string
}

// path returns the filesystem path for a file:// root, or the raw value when it
// is already a plain path.
func (r mcpRoot) path() string {
	if strings.HasPrefix(r.URI, "file://") {
		parsed, err := url.Parse(r.URI)
		if err == nil && parsed.Path != "" {
			return parsed.Path
		}
	}

	return r.URI
}

func rootFromString(raw string) (mcpRoot, bool) {
	uri := strings.TrimSpace(raw)
	if uri == "" {
		return mcpRoot{URI: "", Name: ""}, false
	}

	return mcpRoot{URI: uri, Name: ""}, true
}

// rootHeaderNames are the request headers a proxy/harness may set to hand the server
// the workspace root(s) without the MCP roots round-trip. Values are file://
// URIs or plain paths; multiple roots may be comma-separated.
// X-Repo-Root leads: it is the name the rest of this fleet reads and the one
// the Claude Code entries send, and headers are the only workspace signal that
// survives MCP 2026-07-28 (see resolveRoots).
func rootHeaderNames() []string {
	return []string{"X-Repo-Root", "X-Mcp-Roots", "X-Mcp-Root", "Mcp-Roots", "Mcp-Root"}
}

func parseRootHeaders(h http.Header) []mcpRoot {
	var roots []mcpRoot

	for _, name := range rootHeaderNames() {
		for _, value := range h.Values(name) {
			for part := range strings.SplitSeq(value, ",") {
				if root, ok := rootFromString(part); ok {
					roots = append(roots, root)
				}
			}
		}
	}

	return roots
}

// listRootsTimeout bounds the roots/list round-trip so a client that never
// answers costs one call a few seconds, not the whole tool-call budget.
const listRootsTimeout = 5 * time.Second

// resolveRoots returns the client's workspace roots for the in-flight call.
// Header-pinned roots (set by a proxy/harness over HTTP) take precedence; else
// the roots are fetched from the client session via roots/list.
func resolveRoots(ctx context.Context, req *mcp.CallToolRequest) []mcpRoot {
	if req == nil {
		return nil
	}

	if req.Extra != nil && req.Extra.Header != nil {
		if roots := parseRootHeaders(req.Extra.Header); len(roots) > 0 {
			return roots
		}
	}

	if req.Session == nil {
		return nil
	}
	// Only ask for roots when the client advertised the capability — otherwise
	// ListRoots blocks until timeout against clients that don't support it —
	// and only below rootsRemovedFrom, where a server may still ask at all.
	if !rootsAllowed(req.Session.InitializeParams()) {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, listRootsTimeout)
	defer cancel()

	res, err := req.Session.ListRoots(ctx, &mcp.ListRootsParams{Meta: nil})
	if err != nil || res == nil {
		return nil
	}

	out := make([]mcpRoot, 0, len(res.Roots))
	for _, root := range res.Roots {
		out = append(out, mcpRoot{URI: root.URI, Name: root.Name})
	}

	return out
}

// rootsRemovedFrom is the first protocol revision that forbids server-initiated
// JSON-RPC requests (SEP-2322 / SEP-2575): from there on roots/list is not
// something a server can ask for, only something a tool handler can request via
// InputRequests. Clients on that revision must pin the workspace with one of
// the rootHeaderNames instead. ISO dates compare correctly as strings.
const rootsRemovedFrom = "2026-07-28"

// rootsAllowed reports whether the client behind these initialize params may
// still be asked for its roots: it advertised the capability, on a protocol
// version that still allows the question.
func rootsAllowed(ip *mcp.InitializeParams) bool {
	if ip == nil || ip.Capabilities == nil || ip.ProtocolVersion >= rootsRemovedFrom {
		return false
	}

	return ip.Capabilities.RootsV2 != nil
}
