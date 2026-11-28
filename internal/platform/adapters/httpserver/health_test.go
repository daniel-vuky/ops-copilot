package httpserver

import (
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/daniel-vuky/ops-copilot/internal/platform/logging"
)

func TestReadinessZeroValueIsNotReady(t *testing.T) {
	t.Parallel()

	var rd Readiness
	if rd.Ready() {
		t.Fatal("zero Readiness reports ready")
	}
	rd.SetReady(true)
	if !rd.Ready() {
		t.Fatal("Ready() = false after SetReady(true)")
	}
	rd.SetReady(false)
	if rd.Ready() {
		t.Fatal("Ready() = true after SetReady(false)")
	}
}

func TestHealthzFollowsReadiness(t *testing.T) {
	t.Parallel()

	var rd Readiness
	r := NewRouter(logging.New(io.Discard, slog.LevelDebug), &rd, nil)

	// No Authorization header or cookie is sent: /healthz needs no auth.
	rec := serve(r, http.MethodGet, "/healthz", nil)
	if rec.Code != http.StatusServiceUnavailable || body(t, rec) != "not ready" {
		t.Errorf("not ready: got %d %q, want 503 %q", rec.Code, body(t, rec), "not ready")
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain; charset=utf-8", got)
	}

	rd.SetReady(true)
	rec = serve(r, http.MethodGet, "/healthz", nil)
	if rec.Code != http.StatusOK || body(t, rec) != "ok" {
		t.Errorf("ready: got %d %q, want 200 %q", rec.Code, body(t, rec), "ok")
	}
}

func TestNewRouterNilReadinessIsNotReady(t *testing.T) {
	t.Parallel()

	r := NewRouter(nil, nil, nil)
	if rec := serve(r, http.MethodGet, "/healthz", nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /healthz with nil Readiness = %d, want 503", rec.Code)
	}
}
