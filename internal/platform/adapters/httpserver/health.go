package httpserver

import (
	"net/http"
	"sync/atomic"
)

// Readiness records whether the process is ready to serve. The zero value is
// not ready. It is safe for concurrent use.
type Readiness struct {
	ready atomic.Bool
}

// SetReady marks the process ready (true) or not ready (false).
func (r *Readiness) SetReady(v bool) {
	r.ready.Store(v)
}

// Ready reports whether the process is ready.
func (r *Readiness) Ready() bool {
	return r.ready.Load()
}

// healthHandler answers /healthz: 200 "ok" when ready, 503 "not ready"
// otherwise. It needs no authentication.
func healthHandler(ready *Readiness) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/plain; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		if !ready.Ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready"))
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
}
