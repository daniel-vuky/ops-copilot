// Package httpserver is the HTTP adapter: the route table of the single public
// port (ADR 0001 §2), its middleware, the health endpoint and the server.
package httpserver

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"path"
	"strings"
)

// Route patterns owned by the router.
const (
	healthzPattern = "GET /healthz"
	uiPattern      = "/"
)

// machinePrefixes are the fallback patterns of the machine-facing routes. Any
// path they match and no more specific handler serves gets a JSON 404.
// "/mcp" is exact, so "/mcp/x" belongs to the UI.
var machinePrefixes = []string{"/api/", "/mcp", "/oauth/", "/.well-known/", "/hooks/"}

// Router is the http.Handler of the public port. It wraps an http.ServeMux in
// the middleware chain: request id, security headers, access log, recover,
// then the mux.
type Router struct {
	mux     *http.ServeMux
	handler http.Handler
}

// NewRouter builds the route table. logger is the base logger (nil means
// slog.Default()); ready drives /healthz (nil means never ready); ui serves
// every path no other route claims (nil means a built-in placeholder page).
// The UI handler only answers GET and HEAD.
func NewRouter(logger *slog.Logger, ready *Readiness, ui http.Handler) *Router {
	if logger == nil {
		logger = slog.Default()
	}
	if ready == nil {
		ready = &Readiness{}
	}
	if ui == nil {
		ui = placeholderUI()
	}

	mux := http.NewServeMux()
	mux.Handle(healthzPattern, healthHandler(ready))
	for _, p := range machinePrefixes {
		mux.Handle(p, http.HandlerFunc(notFound))
	}
	// "/" stays method-less: "GET /" would conflict with "/api/" and the other
	// method-less fallbacks, so the method check lives in uiOnlyGet.
	mux.Handle(uiPattern, uiOnlyGet(ui))

	return &Router{
		mux:     mux,
		handler: requestID(logger, securityHeaders(accessLog(recoverPanic(mux)))),
	}
}

// Handle registers h for pattern (http.ServeMux syntax). A more specific
// pattern wins over the fallbacks, so "GET /api/ping" overrides the /api/ 404.
// Like http.ServeMux.Handle, it panics when pattern conflicts with one already
// registered; call it before the server starts.
func (r *Router) Handle(pattern string, h http.Handler) {
	r.mux.Handle(pattern, h)
}

// ServeHTTP runs the middleware chain and the route table.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.handler.ServeHTTP(w, req)
}

// notFound answers a machine-facing path that has no handler.
func notFound(w http.ResponseWriter, _ *http.Request) {
	writeJSONError(w, http.StatusNotFound, "not_found", "not found")
}

// isMachinePath reports whether the cleaned path p belongs to a machine-facing
// route, which answers errors in JSON rather than text.
func isMachinePath(p string) bool {
	for _, prefix := range machinePrefixes {
		if p == strings.TrimSuffix(prefix, "/") || (strings.HasSuffix(prefix, "/") && strings.HasPrefix(p, prefix)) {
			return true
		}
	}
	return false
}

// errorBody is the JSON error envelope: {"error":{"code":..,"message":..}}.
type errorBody struct {
	Error errorDetail `json:"error"`
}

// errorDetail is the inside of errorBody.
type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeJSONError writes status with a JSON error body.
func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	body, err := json.Marshal(errorBody{Error: errorDetail{Code: code, Message: message}})
	if err != nil {
		// Marshalling two strings cannot fail; keep a valid body regardless.
		body = []byte(`{"error":{"code":"internal","message":"internal error"}}`)
	}
	h := w.Header()
	h.Del("Content-Length")
	h.Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// uiOnlyGet lets GET and HEAD through to h and answers any other method with
// 405 and an Allow header.
func uiOnlyGet(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// placeholderHTML is served until the SPA is embedded (SU-07). It is a fixed
// string with no inline script or style, so it works under
// CSP default-src 'self'.
const placeholderHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Ops Copilot</title>
</head>
<body>
<h1>Ops Copilot</h1>
<p>The web interface is not built into this binary yet.</p>
</body>
</html>
`

// placeholderUI serves placeholderHTML for every UI path. The security
// headers come from securityHeaders.
func placeholderUI() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(placeholderHTML))
	})
}

// cleanPath returns the cleaned, rooted form of a request path.
func cleanPath(p string) string {
	return path.Clean("/" + p)
}
