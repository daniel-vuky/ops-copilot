package httpserver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/daniel-vuky/ops-copilot/internal/platform/logging"
)

const (
	notFoundBody = `{"error":{"code":"not_found","message":"not found"}}`
	internalBody = `{"error":{"code":"internal","message":"internal error"}}`
	jsonType     = "application/json; charset=utf-8"
)

// newTestRouter returns a router whose logger writes JSON at debug level into
// the returned buffer. ready sets the initial readiness.
func newTestRouter(t *testing.T, ready bool, ui http.Handler) (*Router, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	rd := &Readiness{}
	rd.SetReady(ready)
	return NewRouter(logging.New(&buf, slog.LevelDebug), rd, ui), &buf
}

// serve sends one request through h and returns the recorded response.
func serve(h http.Handler, method, target string, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// logLines parses every JSON line written to buf.
func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("log line is not JSON: %v: %q", err, sc.Text())
		}
		out = append(out, m)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan log: %v", err)
	}
	return out
}

// accessLines returns the access-log lines among lines.
func accessLines(lines []map[string]any) []map[string]any {
	var out []map[string]any
	for _, l := range lines {
		if l["msg"] == accessLogMessage {
			out = append(out, l)
		}
	}
	return out
}

// body reads the whole recorded body.
func body(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	b, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

func TestRouteTable(t *testing.T) {
	t.Parallel()

	r, _ := newTestRouter(t, true, nil)
	tests := []struct {
		method, target string
		wantStatus     int
		wantType       string
		wantBody       string // exact, when not empty
		wantContains   string // substring, when not empty
		wantAllow      string
		wantCSP        string
	}{
		{method: http.MethodGet, target: "/healthz", wantStatus: 200, wantType: "text/plain; charset=utf-8", wantBody: "ok"},
		{method: http.MethodGet, target: "/api/nope", wantStatus: 404, wantType: jsonType, wantBody: notFoundBody},
		{method: http.MethodPost, target: "/api/x/y", wantStatus: 404, wantType: jsonType, wantBody: notFoundBody},
		{method: http.MethodGet, target: "/mcp", wantStatus: 404, wantType: jsonType, wantBody: notFoundBody},
		{method: http.MethodGet, target: "/oauth/authorize", wantStatus: 404, wantType: jsonType, wantBody: notFoundBody},
		{method: http.MethodGet, target: "/.well-known/x", wantStatus: 404, wantType: jsonType, wantBody: notFoundBody},
		{method: http.MethodPost, target: "/hooks/abc", wantStatus: 404, wantType: jsonType, wantBody: notFoundBody},
		{method: http.MethodGet, target: "/", wantStatus: 200, wantType: "text/html; charset=utf-8", wantContains: "<title>Ops Copilot</title>", wantCSP: "default-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'"},
		{method: http.MethodGet, target: "/investigations/42", wantStatus: 200, wantType: "text/html; charset=utf-8", wantContains: "<title>Ops Copilot</title>"},
		{method: http.MethodGet, target: "/mcp/x", wantStatus: 200, wantType: "text/html; charset=utf-8", wantContains: "<title>Ops Copilot</title>"},
		{method: http.MethodHead, target: "/", wantStatus: 200},
		{method: http.MethodPost, target: "/", wantStatus: 405, wantAllow: "GET, HEAD"},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.target, func(t *testing.T) {
			t.Parallel()

			rec := serve(r, tt.method, tt.target, nil)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantType != "" {
				if got := rec.Header().Get("Content-Type"); got != tt.wantType {
					t.Errorf("Content-Type = %q, want %q", got, tt.wantType)
				}
			}
			got := body(t, rec)
			if tt.wantBody != "" && got != tt.wantBody {
				t.Errorf("body = %q, want %q", got, tt.wantBody)
			}
			if tt.wantContains != "" && !strings.Contains(got, tt.wantContains) {
				t.Errorf("body %q does not contain %q", got, tt.wantContains)
			}
			if tt.wantAllow != "" {
				if allow := rec.Header().Get("Allow"); allow != tt.wantAllow {
					t.Errorf("Allow = %q, want %q", allow, tt.wantAllow)
				}
			}
			if tt.wantCSP != "" {
				if got := rec.Header().Get("Content-Security-Policy"); got != tt.wantCSP {
					t.Errorf("Content-Security-Policy = %q, want %q", got, tt.wantCSP)
				}
			}
			if tt.wantType == jsonType {
				if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
					t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
				}
			}
		})
	}
}

func TestBareNamesRedirect(t *testing.T) {
	t.Parallel()

	r, _ := newTestRouter(t, true, nil)
	for _, name := range []string{"/api", "/hooks", "/oauth"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rec := serve(r, http.MethodGet, name, nil)
			if rec.Code != http.StatusTemporaryRedirect {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusTemporaryRedirect)
			}
			if got := rec.Header().Get("Location"); got != name+"/" {
				t.Errorf("Location = %q, want %q", got, name+"/")
			}
		})
	}
}

func TestHandleAfterConstructionWins(t *testing.T) {
	t.Parallel()

	r, _ := newTestRouter(t, true, nil)
	r.Handle("GET /api/ping", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "pong")
	}))

	rec := serve(r, http.MethodGet, "/api/ping", nil)
	if rec.Code != http.StatusOK || body(t, rec) != "pong" {
		t.Errorf("GET /api/ping = %d %q, want 200 pong", rec.Code, body(t, rec))
	}
	if rec := serve(r, http.MethodGet, "/api/other", nil); rec.Code != http.StatusNotFound {
		t.Errorf("GET /api/other = %d, want 404", rec.Code)
	}
}

func TestCustomUIHandler(t *testing.T) {
	t.Parallel()

	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "spa")
	})
	r, _ := newTestRouter(t, true, ui)

	tests := []struct {
		method, target string
		wantStatus     int
		wantBody       string
	}{
		{http.MethodGet, "/", 200, "spa"},
		{http.MethodGet, "/investigations/42", 200, "spa"},
		{http.MethodPost, "/", 405, ""},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.target, func(t *testing.T) {
			t.Parallel()

			rec := serve(r, tt.method, tt.target, nil)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantBody != "" && body(t, rec) != tt.wantBody {
				t.Errorf("body = %q, want %q", body(t, rec), tt.wantBody)
			}
			if tt.wantStatus == 405 && rec.Header().Get("Allow") != "GET, HEAD" {
				t.Errorf("Allow = %q, want %q", rec.Header().Get("Allow"), "GET, HEAD")
			}
		})
	}
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	t.Parallel()

	// ADR 0002 §6, verbatim: guards the constant against edits.
	const wantCSP = "default-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'"
	if contentSecurityPolicy != wantCSP {
		t.Fatalf("contentSecurityPolicy = %q, want %q", contentSecurityPolicy, wantCSP)
	}

	r, _ := newTestRouter(t, true, nil)
	r.Handle("GET /api/panic", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	tests := []struct {
		name, method, target string
		wantStatus           int
	}{
		{"placeholder UI", http.MethodGet, "/", http.StatusOK},
		{"healthz", http.MethodGet, "/healthz", http.StatusOK},
		{"JSON 404", http.MethodGet, "/api/x", http.StatusNotFound},
		{"UI 405", http.MethodPost, "/", http.StatusMethodNotAllowed},
		{"recovered panic", http.MethodGet, "/api/panic", http.StatusInternalServerError},
		{"clean-path redirect", http.MethodGet, "/a/../b", http.StatusTemporaryRedirect},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := serve(r, tt.method, tt.target, nil)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			h := rec.Header()
			if got := h.Get("Content-Security-Policy"); got != contentSecurityPolicy {
				t.Errorf("Content-Security-Policy = %q, want %q", got, contentSecurityPolicy)
			}
			if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
			if got := h.Get("Referrer-Policy"); got != "same-origin" {
				t.Errorf("Referrer-Policy = %q, want same-origin", got)
			}
		})
	}
}
