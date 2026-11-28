// Command ops-copilot is the single Ops Copilot binary. With no argument it
// loads its configuration from the environment, opens the SQLite store in
// DATA_DIR, requeues the jobs a previous run left running, and serves the
// public HTTP port, with the embedded SPA for every UI path, beside the job
// scheduler until SIGINT or SIGTERM; "ops-copilot version" prints its
// version.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/daniel-vuky/ops-copilot/internal/platform/adapters/httpserver"
	"github.com/daniel-vuky/ops-copilot/internal/platform/adapters/sqlite"
	"github.com/daniel-vuky/ops-copilot/internal/platform/clock"
	"github.com/daniel-vuky/ops-copilot/internal/platform/config"
	"github.com/daniel-vuky/ops-copilot/internal/platform/jobs"
	"github.com/daniel-vuky/ops-copilot/internal/platform/logging"
	"github.com/daniel-vuky/ops-copilot/web"
)

// version can be set at build time with -ldflags "-X main.version=<v>".
// The build does not set it yet (SU-09 will), so it reports "dev".
var version = "dev"

// Exit statuses.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// usage is printed to stderr for an unknown command line.
const usage = "usage: ops-copilot [version]"

// versionString returns the program name followed by its version.
func versionString() string {
	return "ops-copilot " + version
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// After the first signal, restore the default handling so a second one
	// kills the process even if the shutdown hangs.
	context.AfterFunc(ctx, stop)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run is the whole program behind main, with its inputs and outputs passed
// in for tests. It serves until ctx is done and returns the exit status: 0
// after a clean shutdown or "version", 1 for a configuration error, a store
// that fails to open (including a schema newer than this binary), a job
// recovery failure, a listen error, a server or shutdown error, a scheduler
// error or a store close error, 2 for a usage error. The server and the
// scheduler run side by side; when either stops, the other is stopped too.
// The store is closed on every path after it opens, once both have
// returned. The server serves the SPA embedded by package web; a binary
// built without a frontend build logs a warning and serves the placeholder
// page instead.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) (code int) {
	switch {
	case len(args) == 1 && args[0] == "version":
		_, _ = fmt.Fprintln(stdout, versionString())
		return exitOK
	case len(args) != 0:
		_, _ = fmt.Fprintln(stderr, usage)
		return exitUsage
	}

	cfg, err := config.Load(getenv)
	if err != nil {
		// Each joined *config.VarError is "<VAR>: <reason>" on its own line
		// and never holds the raw value.
		_, _ = fmt.Fprintf(stderr, "ops-copilot: invalid configuration:\n%v\n", err)
		return exitError
	}

	logger := logging.New(stdout, cfg.LogLevel)
	slog.SetDefault(logger)
	logger.LogAttrs(ctx, slog.LevelInfo, "config loaded",
		append(cfg.LogAttrs(), slog.String("version", version))...)

	db, err := sqlite.Open(ctx, sqlite.Options{Dir: cfg.DataDir, Logger: logger})
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelError, "store open failed", slog.Any("error", err))
		return exitError
	}
	defer func() {
		if err := db.Close(); err != nil {
			logger.LogAttrs(ctx, slog.LevelError, "store close failed", slog.Any("error", err))
			if code == exitOK {
				code = exitError
			}
		}
	}()
	logger.LogAttrs(ctx, slog.LevelInfo, "store opened", slog.String("path", db.Path()))

	sched := jobs.New(sqlite.NewJobStore(db), db, jobs.Options{Clock: clock.System{}, Logger: logger})
	if err := sched.Recover(ctx); err != nil {
		logger.LogAttrs(ctx, slog.LevelError, "job recovery failed", slog.Any("error", err))
		return exitError
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.ListenAddr)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelError, "listen failed",
			slog.String("addr", cfg.ListenAddr), slog.Any("error", err))
		return exitError
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "listening", slog.String("addr", ln.Addr().String()))

	ui := web.App()
	if ui == nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "web UI not embedded; serving the placeholder page")
	}
	ready := &httpserver.Readiness{}
	router := httpserver.NewRouter(logger, ready, httpserver.SPAHandler(ui))
	srv := httpserver.NewServer(router, ready, logger)
	serve := func(ctx context.Context) error { return srv.Run(ctx, ln) }
	if err := runTogether(ctx, serve, sched.Run); err != nil {
		logger.LogAttrs(ctx, slog.LevelError, "stopped", slog.Any("error", err))
		return exitError
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "stopped")
	return exitOK
}

// runTogether runs server and scheduler until ctx ends or either returns.
// When one returns, the other's context is cancelled; it waits for both and
// returns their errors joined (server first).
func runTogether(ctx context.Context, server, scheduler func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	schedDone := make(chan error, 1)
	go func() {
		defer cancel()
		schedDone <- scheduler(ctx)
	}()

	serverErr := server(ctx)
	cancel()
	return errors.Join(serverErr, <-schedDone)
}
