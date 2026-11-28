package httpserver

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/daniel-vuky/ops-copilot/internal/platform/logging"
)

var requestIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestRequestIDHeader(t *testing.T) {
	t.Parallel()

	r, _ := newTestRouter(t, true, nil)

	first := serve(r, http.MethodGet, "/healthz", http.Header{"X-Request-Id": {"abc"}})
	id1 := first.Header().Get("X-Request-Id")
	if !requestIDPattern.MatchString(id1) {
		t.Fatalf("X-Request-Id = %q, want 32 lowercase hex characters", id1)
	}
	if id1 == "abc" {
		t.Fatal("incoming X-Request-Id was echoed")
	}

	second := serve(r, http.MethodGet, "/healthz", nil)
	if id2 := second.Header().Get("X-Request-Id"); id2 == id1 {
		t.Errorf("two requests got the same id %q", id1)
	}
}

func TestRequestIDInContext(t *testing.T) {
	t.Parallel()

	r, buf := newTestRouter(t, true, nil)
	var seen string
	r.Handle("GET /api/ctx", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		seen = logging.RequestID(req.Context())
		logging.FromContext(req.Context()).Info("x")
		w.WriteHeader(http.StatusNoContent)
	}))

	rec := serve(r, http.MethodGet, "/api/ctx", nil)
	id := rec.Header().Get("X-Request-Id")
	if seen != id {
		t.Errorf("logging.RequestID(ctx) = %q, want header value %q", seen, id)
	}

	var found bool
	for _, l := range logLines(t, buf) {
		if l["msg"] == "x" {
			found = true
			if l["request_id"] != id {
				t.Errorf("handler log request_id = %v, want %q", l["request_id"], id)
			}
		}
	}
	if !found {
		t.Error("no log line written by the handler")
	}
}

func TestAccessLogFields(t *testing.T) {
	t.Parallel()

	r, buf := newTestRouter(t, true, nil)
	rec := serve(r, http.MethodGet, "/api/nope", nil)

	lines := accessLines(logLines(t, buf))
	if len(lines) != 1 {
		t.Fatalf("got %d access-log lines, want 1: %v", len(lines), lines)
	}
	l := lines[0]
	for _, k := range []string{"method", "route", "path", "status", "bytes", "duration_ms", "request_id"} {
		if _, ok := l[k]; !ok {
			t.Errorf("access log has no %q field: %v", k, l)
		}
	}
	checks := map[string]any{
		"level":      "INFO",
		"method":     "GET",
		"route":      "/api/",
		"path":       "/api/nope",
		"status":     float64(404),
		"bytes":      float64(len(notFoundBody)),
		"request_id": rec.Header().Get("X-Request-Id"),
	}
	for k, want := range checks {
		if l[k] != want {
			t.Errorf("%s = %v, want %v", k, l[k], want)
		}
	}
}

func TestAccessLogLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		ready     bool
		target    string
		wantLevel string
	}{
		{name: "healthz ok", ready: true, target: "/healthz", wantLevel: "DEBUG"},
		{name: "healthz not ready", ready: false, target: "/healthz", wantLevel: "INFO"},
		{name: "api not found", ready: true, target: "/api/nope", wantLevel: "INFO"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, buf := newTestRouter(t, tt.ready, nil)
			serve(r, http.MethodGet, tt.target, nil)
			lines := accessLines(logLines(t, buf))
			if len(lines) != 1 {
				t.Fatalf("got %d access-log lines, want 1", len(lines))
			}
			if lines[0]["level"] != tt.wantLevel {
				t.Errorf("level = %v, want %s", lines[0]["level"], tt.wantLevel)
			}
		})
	}
}

func TestAccessLogRedactsHooks(t *testing.T) {
	t.Parallel()

	tests := []struct{ method, target string }{
		{http.MethodPost, "/hooks/secret-token"},
		{http.MethodPost, "//hooks/secret-token"},
		{http.MethodPost, "/hooks/../hooks/secret-token"},
		{http.MethodPost, "/hooks/secret-token/../../x"},
		{http.MethodGet, "/hooks"},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.target, func(t *testing.T) {
			t.Parallel()

			r, buf := newTestRouter(t, true, nil)
			serve(r, tt.method, tt.target, nil)

			if strings.Contains(buf.String(), "secret-token") {
				t.Errorf("log contains the hook token: %s", buf.String())
			}
			lines := accessLines(logLines(t, buf))
			if len(lines) != 1 {
				t.Fatalf("got %d access-log lines, want 1", len(lines))
			}
			if lines[0]["path"] != "/hooks/[redacted]" {
				t.Errorf("path = %v, want /hooks/[redacted]", lines[0]["path"])
			}
		})
	}
}

func TestAccessLogPathCleanedAndCut(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, target string
		check        func(t *testing.T, got string)
	}{
		{
			name:   "cleaned",
			target: "/a/./b/../c",
			check: func(t *testing.T, got string) {
				t.Helper()
				if got != "/a/c" {
					t.Errorf("path = %q, want /a/c", got)
				}
			},
		},
		{
			name:   "long",
			target: "/" + strings.Repeat("a", 4999),
			check: func(t *testing.T, got string) {
				t.Helper()
				if len(got) == 0 || len(got) > maxLoggedPath {
					t.Errorf("logged path has %d bytes, want 1..%d", len(got), maxLoggedPath)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, buf := newTestRouter(t, true, nil)
			serve(r, http.MethodGet, tt.target, nil)
			lines := accessLines(logLines(t, buf))
			if len(lines) != 1 {
				t.Fatalf("got %d access-log lines, want 1", len(lines))
			}
			got, _ := lines[0]["path"].(string)
			tt.check(t, got)
		})
	}
}

func TestLogPath(t *testing.T) {
	t.Parallel()

	long := "/" + strings.Repeat("é", 1000) // 2001 bytes
	tests := []struct {
		raw  string
		want string
	}{
		{"/hooks/abc", redactedHooksPath},
		{"/hooks", redactedHooksPath},
		{"//hooks/abc", redactedHooksPath},
		{"/hooks/abc/../../x", redactedHooksPath},
		{"/api/hooks/x", redactedHooksPath},
		{"/hooksx/abc", "/hooksx/abc"},
		{"", "/"},
		{"/a//b/", "/a/b"},
		{long, long[:1023]}, // cut on a rune boundary: 1 + 511*2 bytes
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			t.Parallel()

			if got := logPath(tt.raw); got != tt.want {
				t.Errorf("logPath(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestRecoverWritesInternalError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, target string
		wantType     string
		wantBody     string
	}{
		{name: "api", target: "/api/boom", wantType: jsonType, wantBody: internalBody},
		{name: "ui", target: "/boom", wantType: "text/plain; charset=utf-8", wantBody: "internal error\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, buf := newTestRouter(t, true, nil)
			r.Handle("GET "+tt.target, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				panic("kaboom-" + tt.name)
			}))

			rec := serve(r, http.MethodGet, tt.target, nil)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != tt.wantType {
				t.Errorf("Content-Type = %q, want %q", got, tt.wantType)
			}
			if got := body(t, rec); got != tt.wantBody {
				t.Errorf("body = %q, want %q", got, tt.wantBody)
			}

			lines := logLines(t, buf)
			var errLine map[string]any
			for _, l := range lines {
				if l["level"] == "ERROR" {
					errLine = l
				}
			}
			if errLine == nil {
				t.Fatal("no ERROR line for the panic")
			}
			if errLine["panic"] != "kaboom-"+tt.name {
				t.Errorf("panic = %v, want %q", errLine["panic"], "kaboom-"+tt.name)
			}
			acc := accessLines(lines)
			if len(acc) != 1 || acc[0]["status"] != float64(500) {
				t.Errorf("access log = %v, want one line with status 500", acc)
			}
			if len(acc) == 1 && acc[0]["aborted"] != nil {
				t.Errorf("aborted = %v, want absent for a recovered panic", acc[0]["aborted"])
			}

			if rec := serve(r, http.MethodGet, "/healthz", nil); rec.Code != http.StatusOK {
				t.Errorf("GET /healthz after panic = %d, want 200", rec.Code)
			}
		})
	}
}

// countingRecorder counts WriteHeader calls that reach the recorder.
type countingRecorder struct {
	*httptest.ResponseRecorder
	writeHeaders int
}

func (c *countingRecorder) WriteHeader(code int) {
	c.writeHeaders++
	c.ResponseRecorder.WriteHeader(code)
}

// serveExpectAbort sends one request through h and fails unless ServeHTTP
// panics with http.ErrAbortHandler.
func serveExpectAbort(t *testing.T, h http.Handler, w http.ResponseWriter, target string) {
	t.Helper()
	defer func() {
		v := recover()
		err, ok := v.(error)
		if !ok || !errors.Is(err, http.ErrAbortHandler) {
			t.Errorf("recovered %v, want http.ErrAbortHandler", v)
		}
	}()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	t.Error("ServeHTTP returned normally, want an http.ErrAbortHandler panic")
}

func TestRecoverAfterResponseStarted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		handler          http.HandlerFunc
		wantBody         string
		wantWriteHeaders int // calls made by the handler itself
	}{
		{
			name: "partial write",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, "partial")
				panic("late")
			},
			wantBody:         "partial",
			wantWriteHeaders: 1,
		},
		{
			name: "flushed",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.(http.Flusher).Flush()
				panic("late")
			},
			wantBody:         "",
			wantWriteHeaders: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, buf := newTestRouter(t, true, nil)
			r.Handle("GET /api/late", tt.handler)

			rec := &countingRecorder{ResponseRecorder: httptest.NewRecorder()}
			serveExpectAbort(t, r, rec, "/api/late")
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", rec.Code)
			}
			if got := rec.Body.String(); got != tt.wantBody {
				t.Errorf("body = %q, want %q", got, tt.wantBody)
			}
			if rec.writeHeaders != tt.wantWriteHeaders {
				t.Errorf("WriteHeader calls = %d, want %d (no second WriteHeader)", rec.writeHeaders, tt.wantWriteHeaders)
			}
			lines := logLines(t, buf)
			var sawError bool
			for _, l := range lines {
				sawError = sawError || (l["level"] == "ERROR" && l["panic"] == "late")
			}
			if !sawError {
				t.Error("no ERROR line for the panic")
			}
			acc := accessLines(lines)
			if len(acc) != 1 || acc[0]["status"] != float64(200) || acc[0]["aborted"] != true {
				t.Errorf("access log = %v, want one line with status 200 and aborted true", acc)
			}

			if rec := serve(r, http.MethodGet, "/healthz", nil); rec.Code != http.StatusOK {
				t.Errorf("GET /healthz after abort = %d, want 200", rec.Code)
			}
		})
	}
}

func TestRecoverInvalidWriteHeaderCode(t *testing.T) {
	t.Parallel()

	r, buf := newTestRouter(t, true, nil)
	r.Handle("GET /api/zero", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(0)
	}))

	rec := serve(r, http.MethodGet, "/api/zero", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := body(t, rec); got != internalBody {
		t.Errorf("body = %q, want %q", got, internalBody)
	}
	acc := accessLines(logLines(t, buf))
	if len(acc) != 1 || acc[0]["status"] != float64(500) {
		t.Errorf("access log = %v, want one line with status 500", acc)
	}
}

func TestRecoverDropsContentHeaders(t *testing.T) {
	t.Parallel()

	r, _ := newTestRouter(t, true, nil)
	r.Handle("GET /api/cached", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "public, max-age=3600")
		h.Set("ETag", `"v1"`)
		h.Set("Last-Modified", "Mon, 02 Jan 2026 03:04:05 GMT")
		h.Set("Content-Encoding", "gzip")
		h.Set("Content-Length", "99")
		panic("boom")
	}))

	rec := serve(r, http.MethodGet, "/api/cached", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	for _, k := range []string{"ETag", "Last-Modified", "Content-Encoding", "Content-Length"} {
		if got := rec.Header().Get(k); got != "" {
			t.Errorf("%s = %q, want it removed", k, got)
		}
	}
	if got := body(t, rec); got != internalBody {
		t.Errorf("body = %q, want %q", got, internalBody)
	}
}

func TestRecoverRepanicsAbortHandler(t *testing.T) {
	t.Parallel()

	r, buf := newTestRouter(t, true, nil)
	r.Handle("GET /api/abort", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	serveExpectAbort(t, r, httptest.NewRecorder(), "/api/abort")

	lines := logLines(t, buf)
	for _, l := range lines {
		if l["level"] == "ERROR" {
			t.Errorf("unexpected ERROR line: %v", l)
		}
	}
	// Nothing was written before the abort, so no status was sent: 0, not 200.
	if acc := accessLines(lines); len(acc) != 1 || acc[0]["status"] != float64(0) || acc[0]["aborted"] != true {
		t.Errorf("access log = %v, want one line with status 0 and aborted true", acc)
	}

	if rec := serve(r, http.MethodGet, "/healthz", nil); rec.Code != http.StatusOK {
		t.Errorf("GET /healthz after abort = %d, want 200", rec.Code)
	}
}

func TestHandlerWithoutWriteLoggedAs200(t *testing.T) {
	t.Parallel()

	r, buf := newTestRouter(t, true, nil)
	r.Handle("GET /api/silent", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	rec := serve(r, http.MethodGet, "/api/silent", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	acc := accessLines(logLines(t, buf))
	if len(acc) != 1 || acc[0]["status"] != float64(200) || acc[0]["bytes"] != float64(0) {
		t.Errorf("access log = %v, want one line with status 200 and 0 bytes", acc)
	}
}

func TestFlushThroughWrapper(t *testing.T) {
	t.Parallel()

	r, _ := newTestRouter(t, true, nil)
	r.Handle("GET /api/flush", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "chunk")
		w.(http.Flusher).Flush()
	}))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/flush", nil))
	if !rec.Flushed {
		t.Error("recorder was not flushed")
	}
	if rec.Body.String() != "chunk" {
		t.Errorf("body = %q, want chunk", rec.Body.String())
	}
}

func TestResponseWriterStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		do         func(w http.ResponseWriter)
		wantStatus int
		wantBytes  int64
		wantHeader bool
	}{
		{name: "nothing", do: func(http.ResponseWriter) {}, wantStatus: 0, wantHeader: false},
		{name: "write", do: func(w http.ResponseWriter) { _, _ = io.WriteString(w, "abc") }, wantStatus: 200, wantBytes: 3, wantHeader: true},
		{name: "explicit", do: func(w http.ResponseWriter) { w.WriteHeader(http.StatusTeapot) }, wantStatus: 418, wantHeader: true},
		{name: "informational then final", do: func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusEarlyHints)
			w.WriteHeader(http.StatusCreated)
		}, wantStatus: 201, wantHeader: true},
		{name: "second WriteHeader ignored", do: func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusAccepted)
			w.WriteHeader(http.StatusBadGateway)
		}, wantStatus: 202, wantHeader: true},
		{name: "flush", do: func(w http.ResponseWriter) { w.(http.Flusher).Flush() }, wantStatus: 200, wantHeader: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rw := &responseWriter{ResponseWriter: httptest.NewRecorder()}
			tt.do(rw)
			if rw.status != tt.wantStatus || rw.bytes != tt.wantBytes || rw.wroteHeader != tt.wantHeader {
				t.Errorf("status, bytes, wroteHeader = %d, %d, %v; want %d, %d, %v",
					rw.status, rw.bytes, rw.wroteHeader, tt.wantStatus, tt.wantBytes, tt.wantHeader)
			}
			if rw.Unwrap() == nil {
				t.Error("Unwrap() = nil")
			}
		})
	}
}

// plainWriter is a ResponseWriter that cannot flush.
type plainWriter struct{ h http.Header }

func (p *plainWriter) Header() http.Header         { return p.h }
func (p *plainWriter) Write(b []byte) (int, error) { return len(b), nil }
func (p *plainWriter) WriteHeader(int)             {}

func TestFlushError(t *testing.T) {
	t.Parallel()

	t.Run("supported", func(t *testing.T) {
		t.Parallel()

		rec := httptest.NewRecorder()
		rw := &responseWriter{ResponseWriter: rec}
		if err := http.NewResponseController(rw).Flush(); err != nil {
			t.Fatalf("Flush: %v", err)
		}
		if !rec.Flushed || !rw.wroteHeader || rw.status != http.StatusOK {
			t.Errorf("Flushed, wroteHeader, status = %v, %v, %d; want true, true, 200", rec.Flushed, rw.wroteHeader, rw.status)
		}
	})
	t.Run("not supported", func(t *testing.T) {
		t.Parallel()

		rw := &responseWriter{ResponseWriter: &plainWriter{h: http.Header{}}}
		err := rw.FlushError()
		if !errors.Is(err, http.ErrNotSupported) {
			t.Errorf("FlushError = %v, want http.ErrNotSupported", err)
		}
		if rw.wroteHeader {
			t.Error("failed flush marked the headers written")
		}
	})
}

func TestSecurityHeadersSetBeforeNext(t *testing.T) {
	t.Parallel()

	var seen http.Header
	h := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		seen = w.Header().Clone()
	}))
	serve(h, http.MethodGet, "/", nil)

	want := map[string]string{
		"Content-Security-Policy": contentSecurityPolicy,
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "same-origin",
	}
	for k, v := range want {
		if got := seen.Get(k); got != v {
			t.Errorf("next saw %s = %q, want %q", k, got, v)
		}
	}
}
