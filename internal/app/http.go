package app

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultHTTPAddr = "127.0.0.1:8765"
	defaultHTTPPath = "/mcp"

	sessionTTL = 30 * time.Minute

	// readHeaderTimeout bounds slow-loris clients; shutdownTimeout is how long
	// in-flight requests get to drain on SIGINT/SIGTERM.
	readHeaderTimeout = 10 * time.Second
	shutdownTimeout   = 5 * time.Second
)

// authMiddleware enforces the bearer token (LARAVEL_MCP_TOKEN) when configured.
// When no token is set it is a no-op, preserving the zero-config local flow.
func authMiddleware(next http.Handler) http.Handler {
	want := cfg.AuthToken
	if want == "" {
		return next
	}

	return http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		got := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		if got == "" {
			got = req.Header.Get("X-Mcp-Token")
		}

		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			writer.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(writer, "unauthorized", http.StatusUnauthorized)

			return
		}

		next.ServeHTTP(writer, req)
	})
}

func ensureLeadingSlash(path string) string {
	if path == "" {
		return defaultHTTPPath
	}

	if !strings.HasPrefix(path, "/") {
		return "/" + path
	}

	return path
}

// serveHTTP runs the MCP server over the SDK's Streamable HTTP transport on a
// single endpoint, with idle-session reaping. The handler exposes request
// headers to tool handlers (header-pinned roots). Shuts down when ctx is
// cancelled. A returned error is ready to log as-is.
func serveHTTP(ctx context.Context, getServer func(*http.Request) *mcp.Server, addr, path string) error {
	// The SDK defaults are what we want for everything but the session TTL.
	opts := new(mcp.StreamableHTTPOptions)
	opts.SessionTimeout = sessionTTL

	handler := mcp.NewStreamableHTTPHandler(getServer, opts)

	mux := http.NewServeMux()
	mux.Handle(path, authMiddleware(handler))
	mux.HandleFunc("/healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{"status":"ok"}`))
	})

	// net/http defaults for everything else; only the header timeout differs.
	httpSrv := new(http.Server)
	httpSrv.Addr = addr
	httpSrv.Handler = mux
	httpSrv.ReadHeaderTimeout = readHeaderTimeout

	go func() {
		<-ctx.Done()
		// ctx is already done here; WithoutCancel keeps its values but gives
		// Shutdown its own deadline to drain in-flight requests.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()

		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	logf("listening on http://%s%s (MCP Streamable HTTP)", addr, path)

	err := httpSrv.ListenAndServe()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server error: %w", err)
	}

	return nil
}
