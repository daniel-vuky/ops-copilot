package httpserver

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/daniel-vuky/ops-copilot/internal/platform/logging"
)

const (
	// requestIDHeader is the response header that carries the request id.
	requestIDHeader = "X-Request-Id"
	// accessLogMessage is the message of every access-log line.
	accessLogMessage = "http request"
	// maxLoggedPath is the most bytes of a request path the access log keeps.
	maxLoggedPath = 1024
	// redactedHooksPath replaces every webhook path in the access log, since
	// the webhook token is part of the path (ADR 0002).
	redactedHooksPath = "/hooks/[redacted]"
	// contentSecurityPolicy is the Content-Security-Policy of ADR 0002 §6,
	// sent on every response.
	contentSecurityPolicy = "default-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'"
)

// requestID gives each request a fresh random id, sets it on the response and
// puts it, with a logger that carries it, into the request context. The
// incoming X-Request-Id header is ignored. This is the only layer that calls
// WithContext: the inner layers share one *http.Request, so the access log
// can read the Pattern the mux sets on it.
func requestID(base *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		w.Header().Set(requestIDHeader, id)
		ctx := logging.WithLogger(r.Context(), base.With(slog.String("request_id", id)))
		ctx = logging.WithRequestID(ctx, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// securityHeaders sets Content-Security-Policy (ADR 0002 §6),
// X-Content-Type-Options: nosniff and Referrer-Policy: same-origin on the
// response before next runs, so every response that passes through the chain
// carries them: handler output, ServeMux redirects and 404s, and the 500 of
// recoverPanic. Errors net/http writes before any handler runs (such as 400
// for a malformed request or 431 for oversized headers) never reach the chain
// and carry none of them.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// newRequestID returns 16 random bytes as 32 lowercase hex characters.
func newRequestID() string {
	var b [16]byte
	// crypto/rand.Read never returns an error: it crashes the program instead.
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// accessLog writes one line per request once the inner handler is done, even
// when it panics. A 200 from /healthz is logged at debug level, every other
// response at info level. A line written while a panic is passing through
// (such as a re-panicked http.ErrAbortHandler) carries aborted=true, and
// status 0 when no response was started.
func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w}
		completed := false
		defer func() {
			status := rw.status
			if !rw.wroteHeader && completed {
				// net/http sends 200 when a handler returns without writing.
				// When a panic passes with nothing written, no response is
				// sent at all, so status stays 0.
				status = http.StatusOK
			}
			level := slog.LevelInfo
			if r.Pattern == healthzPattern && status == http.StatusOK {
				level = slog.LevelDebug
			}
			attrs := []slog.Attr{
				slog.String("method", r.Method),
				slog.String("route", r.Pattern),
				slog.String("path", logPath(r.URL.Path)),
				slog.Int("status", status),
				slog.Int64("bytes", rw.bytes),
				slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
			}
			if !completed {
				// The inner handler did not return: a panic is passing through.
				attrs = append(attrs, slog.Bool("aborted", true))
			}
			logging.FromContext(r.Context()).LogAttrs(r.Context(), level, accessLogMessage, attrs...)
		}()
		next.ServeHTTP(rw, r)
		completed = true
	})
}

// logPath returns the form of a request path that may be logged. Any path
// that is, or may be routed or redirected to, a webhook is replaced by
// redactedHooksPath. The rule is broad on purpose: it also redacts paths such
// as /api/hooks, which the route field still identifies; UC-12 narrows it.
// Other paths are cleaned, made valid UTF-8 and cut to maxLoggedPath bytes.
func logPath(raw string) string {
	clean := cleanPath(raw)
	if clean == "/hooks" || strings.HasPrefix(clean, "/hooks/") || hasSegment(raw, "hooks") {
		return redactedHooksPath
	}
	return truncateUTF8(strings.ToValidUTF8(clean, "�"), maxLoggedPath)
}

// hasSegment reports whether one of the slash-separated segments of p is seg.
func hasSegment(p, seg string) bool {
	for s := range strings.SplitSeq(p, "/") {
		if s == seg {
			return true
		}
	}
	return false
}

// truncateUTF8 cuts the valid UTF-8 string s to at most n bytes without
// splitting a rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// recoverPanic turns a panic in next into a logged error and, when the
// response has not started, a 500: JSON on machine-facing paths, text on UI
// paths. When the response has started, it panics with http.ErrAbortHandler
// after logging, so net/http drops the connection instead of ending a
// truncated body as if it were complete. http.ErrAbortHandler from next is
// re-panicked unlogged, as net/http expects.
func recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(v)
			}
			logging.FromContext(r.Context()).LogAttrs(r.Context(), slog.LevelError, "panic serving request",
				slog.String("panic", fmt.Sprint(v)),
				slog.String("stack", string(debug.Stack())),
			)
			if headerWritten(w) {
				panic(http.ErrAbortHandler)
			}
			// Drop headers that described the content the handler meant to
			// send, and keep the error out of caches. The security headers
			// set by securityHeaders are left in place on purpose.
			h := w.Header()
			for _, k := range []string{"Content-Encoding", "Content-Length", "ETag", "Last-Modified"} {
				h.Del(k)
			}
			h.Set("Cache-Control", "no-store")
			if isMachinePath(cleanPath(r.URL.Path)) {
				writeJSONError(w, http.StatusInternalServerError, "internal", "internal error")
				return
			}
			http.Error(w, "internal error", http.StatusInternalServerError)
		}()
		next.ServeHTTP(w, r)
	})
}

// headerWritten reports whether the response headers of w have been sent.
// A writer that is not a *responseWriter is assumed unsent.
func headerWritten(w http.ResponseWriter) bool {
	rw, ok := w.(*responseWriter)
	return ok && rw.wroteHeader
}

// responseWriter records the status, the body size and whether the headers
// were sent, for the access log and the recover layer.
type responseWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

// WriteHeader passes every call on, then records the first final status.
// Informational 1xx codes other than 101 are passed on but not recorded,
// since the final status follows them. The call is delegated first, so an
// invalid code panics before any state changes and the recover layer can
// still answer 500.
func (w *responseWriter) WriteHeader(code int) {
	w.ResponseWriter.WriteHeader(code)
	if !w.wroteHeader && (code > 199 || code == http.StatusSwitchingProtocols) {
		w.status = code
		w.wroteHeader = true
	}
}

// Write sends the body, recording an implicit 200 on the first call.
func (w *responseWriter) Write(b []byte) (int, error) {
	w.markWritten()
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Flush sends buffered data to the client, ignoring any error; see FlushError.
func (w *responseWriter) Flush() {
	_ = w.FlushError()
}

// FlushError sends buffered data to the client and returns any error, for
// http.ResponseController. A successful flush sends the headers too, so it
// records an implicit 200 when no status was written.
func (w *responseWriter) FlushError() error {
	if err := http.NewResponseController(w.ResponseWriter).Flush(); err != nil {
		return fmt.Errorf("flush response: %w", err)
	}
	w.markWritten()
	return nil
}

// Unwrap returns the wrapped writer, for http.ResponseController.
func (w *responseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// markWritten records an implicit 200 when no status was written yet.
func (w *responseWriter) markWritten() {
	if !w.wroteHeader {
		w.status = http.StatusOK
		w.wroteHeader = true
	}
}
