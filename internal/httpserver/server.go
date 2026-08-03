// Package httpserver wires the MCP Streamable HTTP handler behind a single
// mux with bearer-token auth and an unauthenticated /healthz endpoint.
package httpserver

import (
	"net/http"
)

// New builds the top-level HTTP handler:
//   - GET /healthz — unauthenticated liveness check.
//   - /mcp        — MCP Streamable HTTP endpoint, bearer-auth-gated.
//
// token is the shared secret that MCP clients must present.
func New(mcpHandler http.Handler, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("/mcp", requireBearerToken(token, mcpHandler))
	return mux
}
