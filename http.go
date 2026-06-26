package main

import (
	"context"
	"crypto/subtle"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultHTTPAddr = "127.0.0.1:8765"
	defaultHTTPPath = "/mcp"

	sessionTTL = 30 * time.Minute
)

// httpAddr resolves the HTTP listen address. HTTP mode is enabled when --http
// (optionally --http=addr) is passed or LARAVEL_MCP_HTTP_ADDR is set. A bare
// --http or a truthy LARAVEL_MCP_HTTP uses defaultHTTPAddr. Returns "" for the
// default stdio transport.
func httpAddr() string {
	args := os.Args[1:]
	for i, a := range args {
		switch a {
		case "--http":
			if next := args[i+1:]; len(next) > 0 && !strings.HasPrefix(next[0], "-") && strings.Contains(next[0], ":") {
				return next[0]
			}
			return defaultHTTPAddr
		default:
			if v, ok := strings.CutPrefix(a, "--http="); ok {
				if v != "" {
					return v
				}
				return defaultHTTPAddr
			}
		}
	}
	if v := os.Getenv("LARAVEL_MCP_HTTP_ADDR"); v != "" {
		return v
	}
	if truthy(os.Getenv("LARAVEL_MCP_HTTP")) {
		return defaultHTTPAddr
	}
	return ""
}

// httpPath resolves the endpoint path (--http-path / LARAVEL_MCP_HTTP_PATH).
func httpPath() string {
	args := os.Args[1:]
	for i, a := range args {
		if a == "--http-path" {
			if next := args[i+1:]; len(next) > 0 {
				return ensureLeadingSlash(next[0])
			}
		}
		if v, ok := strings.CutPrefix(a, "--http-path="); ok {
			return ensureLeadingSlash(v)
		}
	}
	if v := os.Getenv("LARAVEL_MCP_HTTP_PATH"); v != "" {
		return ensureLeadingSlash(v)
	}
	return defaultHTTPPath
}

// authMiddleware enforces the bearer token (LARAVEL_MCP_TOKEN) when configured.
// When no token is set it is a no-op, preserving the zero-config local flow.
func authMiddleware(next http.Handler) http.Handler {
	want := cfg.AuthToken
	if want == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" {
			got = r.Header.Get("X-Mcp-Token")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func ensureLeadingSlash(p string) string {
	if p == "" {
		return defaultHTTPPath
	}
	if !strings.HasPrefix(p, "/") {
		return "/" + p
	}
	return p
}

// serveHTTP runs the MCP server over the SDK's Streamable HTTP transport on a
// single endpoint, with idle-session reaping. The handler exposes request
// headers to tool handlers (header-pinned roots). Shuts down when ctx is
// cancelled.
func serveHTTP(ctx context.Context, srv *mcp.Server, addr, path string) error {
	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{SessionTimeout: sessionTTL},
	)

	mux := http.NewServeMux()
	mux.Handle(path, authMiddleware(handler))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	httpSrv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	logf("listening on http://%s%s (MCP Streamable HTTP)", addr, path)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
