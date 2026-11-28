package httpserver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/daniel-vuky/ops-copilot/internal/platform/logging"
)

// waitTimeout bounds every wait in these tests.
const waitTimeout = 2 * time.Second

// listenLocal opens a listener on a free loopback port.
func listenLocal(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

// newTestClient returns a client with its own transport, closed at cleanup.
func newTestClient(t *testing.T) *http.Client {
	t.Helper()
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

// slowRouter returns a router whose GET /api/slow closes started, waits on
// release and then answers "done".
func slowRouter(ready *Readiness, started chan<- struct{}, release <-chan struct{}) *Router {
	r := NewRouter(logging.New(io.Discard, slog.LevelDebug), ready, nil)
	r.Handle("GET /api/slow", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = w.Write([]byte("done"))
	}))
	return r
}

// result is the outcome of one client request.
type result struct {
	status int
	body   string
	err    error
}

// get sends a GET to url and reads the whole body.
func get(c *http.Client, url string) result {
	resp, err := c.Get(url)
	if err != nil {
		return result{err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return result{status: resp.StatusCode, body: string(b), err: err}
}

func TestServerSettings(t *testing.T) {
	t.Parallel()

	s := NewServer(http.NotFoundHandler(), nil, nil)
	srv := s.HTTP()
	if srv.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 10s", srv.ReadHeaderTimeout)
	}
	if srv.IdleTimeout != 120*time.Second {
		t.Errorf("IdleTimeout = %v, want 120s", srv.IdleTimeout)
	}
	if srv.MaxHeaderBytes != 1<<20 {
		t.Errorf("MaxHeaderBytes = %d, want %d", srv.MaxHeaderBytes, 1<<20)
	}
	if srv.TLSConfig != nil {
		t.Error("TLSConfig is set, want nil (TLS belongs to the proxy)")
	}
	if srv.ErrorLog == nil {
		t.Error("ErrorLog is nil, want a logger backed by slog")
	}
	if s.ShutdownTimeout != 8*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 8s", s.ShutdownTimeout)
	}
}

func TestRunServesAndShutsDownGracefully(t *testing.T) {
	t.Parallel()

	ln := listenLocal(t)
	base := "http://" + ln.Addr().String()

	ready := &Readiness{}
	started := make(chan struct{})
	release := make(chan struct{})
	s := NewServer(slowRouter(ready, started, release), ready, logging.New(io.Discard, slog.LevelDebug))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx, ln) }()

	client := newTestClient(t)
	// The listener is bound before Run starts and ready is set before Serve,
	// so this request waits in the backlog and then sees a ready server.
	if r := get(client, base+"/healthz"); r.err != nil || r.status != http.StatusOK || r.body != "ok" {
		t.Fatalf("GET /healthz = %d %q (err %v), want 200 %q", r.status, r.body, r.err, "ok")
	}

	slow := make(chan result, 1)
	go func() { slow <- get(client, base+"/api/slow") }()
	select {
	case <-started:
	case <-time.After(waitTimeout):
		t.Fatal("slow handler did not start")
	}

	cancel()
	// Allowed poll: readiness turns false asynchronously after cancel.
	deadline := time.Now().Add(waitTimeout)
	for ready.Ready() {
		if time.Now().After(deadline) {
			t.Fatal("readiness still true 2s after cancel")
		}
		time.Sleep(5 * time.Millisecond)
	}

	close(release)
	select {
	case r := <-slow:
		if r.err != nil || r.status != http.StatusOK || r.body != "done" {
			t.Errorf("in-flight request = %d %q (err %v), want 200 %q", r.status, r.body, r.err, "done")
		}
	case <-time.After(waitTimeout):
		t.Fatal("in-flight request did not finish")
	}

	select {
	case err := <-runErr:
		if err != nil {
			t.Errorf("Run() = %v, want nil", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Run did not return after shutdown")
	}

	// Shutdown closes the listener. Checking the listener itself, rather than
	// dialing the freed port, cannot be fooled by another test reusing it.
	// The deadline keeps the test from hanging if Run ever stops closing it.
	if tl, ok := ln.(*net.TCPListener); ok {
		_ = tl.SetDeadline(time.Now().Add(waitTimeout)) // fails on a closed listener, as wanted
	}
	if conn, err := ln.Accept(); !errors.Is(err, net.ErrClosed) {
		if conn != nil {
			_ = conn.Close()
		}
		t.Errorf("Accept after shutdown = %v, want an error wrapping net.ErrClosed", err)
	}
}

func TestRunReportsMissedShutdownDeadline(t *testing.T) {
	t.Parallel()

	ln := listenLocal(t)
	base := "http://" + ln.Addr().String()

	ready := &Readiness{}
	started := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	s := NewServer(slowRouter(ready, started, release), ready, logging.New(io.Discard, slog.LevelDebug))
	s.ShutdownTimeout = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx, ln) }()

	client := newTestClient(t)
	slow := make(chan result, 1)
	go func() { slow <- get(client, base+"/api/slow") }()
	select {
	case <-started:
	case <-time.After(waitTimeout):
		t.Fatal("slow handler did not start")
	}

	cancel()
	select {
	case err := <-runErr:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Run() = %v, want an error wrapping context.DeadlineExceeded", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Run did not return after the shutdown deadline")
	}

	// Close dropped the in-flight connection, so the client sees an error.
	select {
	case r := <-slow:
		if r.err == nil {
			t.Errorf("in-flight request = %d %q, want a connection error after Close", r.status, r.body)
		}
	case <-time.After(waitTimeout):
		t.Fatal("in-flight request still pending after Close")
	}
}

func TestRunReportsServeError(t *testing.T) {
	t.Parallel()

	ln := listenLocal(t)
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	ready := &Readiness{}
	s := NewServer(http.NotFoundHandler(), ready, logging.New(io.Discard, slog.LevelDebug))

	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(context.Background(), ln) }()
	select {
	case err := <-runErr:
		if err == nil {
			t.Fatal("Run() on a closed listener = nil, want an error")
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Run() = %v, want a serve error, not a missed deadline", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Run did not return on a closed listener")
	}
	if ready.Ready() {
		t.Error("readiness still true after Serve failed")
	}
}

func TestRunDefaultsNonPositiveShutdownTimeout(t *testing.T) {
	t.Parallel()

	for _, timeout := range []time.Duration{0, -time.Second} {
		t.Run(timeout.String(), func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			s := NewServer(http.NotFoundHandler(), nil, logging.New(&buf, slog.LevelDebug))
			s.ShutdownTimeout = timeout

			// A cancelled ctx sends Run straight to the shutdown path: Serve
			// on an open listener does not return before Shutdown.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			ln := listenLocal(t)
			runErr := make(chan error, 1)
			go func() { runErr <- s.Run(ctx, ln) }()
			select {
			case err := <-runErr:
				if err != nil {
					t.Fatalf("Run() = %v, want nil", err)
				}
			case <-time.After(waitTimeout):
				t.Fatal("Run did not return after cancel")
			}

			var found bool
			for _, l := range logLines(t, &buf) {
				if l["msg"] == "shutting down" {
					found = true
					if l["timeout"] != "8s" {
						t.Errorf("shutdown timeout = %v, want %q", l["timeout"], "8s")
					}
				}
			}
			if !found {
				t.Errorf("no %q log line: %q", "shutting down", buf.String())
			}
		})
	}
}
