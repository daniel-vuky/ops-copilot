package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Server settings (ADR 0001 §2: plain HTTP on one port, TLS belongs to the
// proxy in front).
const (
	readHeaderTimeout      = 10 * time.Second
	idleTimeout            = 120 * time.Second
	maxHeaderBytes         = 1 << 20
	defaultShutdownTimeout = 8 * time.Second
)

// Server runs the public HTTP server and drains it on shutdown. Readiness is
// true while it serves and turns false when shutdown starts; from then on new
// connections are refused while in-flight requests finish.
type Server struct {
	// ShutdownTimeout bounds the graceful shutdown. When in-flight requests
	// take longer, the remaining connections are closed. NewServer sets it to
	// 8 s, and Run treats zero or a negative value as 8 s too.
	ShutdownTimeout time.Duration

	srv    *http.Server
	ready  *Readiness
	logger *slog.Logger
}

// NewServer returns a server for h. ready is set while the server runs (nil
// means a private Readiness); logger receives the net/http error log at error
// level (nil means slog.Default()).
func NewServer(h http.Handler, ready *Readiness, logger *slog.Logger) *Server {
	if ready == nil {
		ready = &Readiness{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		ShutdownTimeout: defaultShutdownTimeout,
		srv: &http.Server{
			Handler:           h,
			ReadHeaderTimeout: readHeaderTimeout,
			IdleTimeout:       idleTimeout,
			MaxHeaderBytes:    maxHeaderBytes,
			ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
		},
		ready:  ready,
		logger: logger,
	}
}

// HTTP returns the underlying *http.Server, for tests.
func (s *Server) HTTP() *http.Server {
	return s.srv
}

// Run serves on ln until ctx is done, then shuts down gracefully within
// ShutdownTimeout. It returns nil after a clean shutdown, an error wrapping
// context.DeadlineExceeded when in-flight requests outlived the deadline (the
// remaining connections are then closed), or the error that stopped Serve.
// Run takes ownership of ln and closes it.
func (s *Server) Run(ctx context.Context, ln net.Listener) error {
	// Buffered so the Serve goroutine never blocks when Run has already
	// returned on the shutdown path.
	serveErr := make(chan error, 1)
	s.ready.SetReady(true)
	go func() { serveErr <- s.srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		s.ready.SetReady(false)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		_ = s.srv.Close()
		return fmt.Errorf("serve http: %w", err)
	case <-ctx.Done():
	}

	s.ready.SetReady(false)
	timeout := s.shutdownTimeout()
	s.logger.LogAttrs(ctx, slog.LevelInfo, "shutting down",
		slog.String("timeout", timeout.String()))

	// ctx is already cancelled; keep its values but not its cancellation.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	if err := s.srv.Shutdown(shutdownCtx); err != nil {
		closeErr := s.srv.Close()
		return errors.Join(fmt.Errorf("shut down http server within %s: %w", timeout, err), closeErr)
	}

	// Shutdown makes Serve return ErrServerClosed at once; anything else is
	// a real serve error that raced with the shutdown.
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve http: %w", err)
	}
	return nil
}

// shutdownTimeout returns ShutdownTimeout, or the 8 s default when it is zero
// or negative (a zero timeout would cut every in-flight request at once).
func (s *Server) shutdownTimeout() time.Duration {
	if s.ShutdownTimeout <= 0 {
		return defaultShutdownTimeout
	}
	return s.ShutdownTimeout
}
