package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver for fixtures

	"github.com/daniel-vuky/ops-copilot/internal/platform/adapters/sqlite"
)

// waitTimeout bounds every wait in these tests.
const waitTimeout = 2 * time.Second

func TestVersionString(t *testing.T) {
	if got, want := versionString(), "ops-copilot dev"; got != want {
		t.Fatalf("versionString() = %q, want %q", got, want)
	}
}

// envOf returns a getenv function backed by m.
func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// keepDefaultLogger restores slog's default logger after the test, since run
// replaces it.
func keepDefaultLogger(t *testing.T) {
	t.Helper()
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
}

// freeAddr returns a loopback address with a port that was free a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return addr
}

// jsonLines decodes every line of buf as a JSON object.
func jsonLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", sc.Text(), err)
		}
		out = append(out, m)
	}
	return out
}

// findLine returns the first log line whose msg is msg.
func findLine(lines []map[string]any, msg string) (map[string]any, bool) {
	for _, l := range lines {
		if l["msg"] == msg {
			return l, true
		}
	}
	return nil, false
}

// serveUntilHealthy runs the program with env until /healthz on its
// LISTEN_ADDR answers 200, then cancels it and returns its exit status and
// output.
func serveUntilHealthy(t *testing.T, env func(string) string) (code int, stdout, stderr *bytes.Buffer) {
	t.Helper()
	addr := env("LISTEN_ADDR")
	stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- run(ctx, nil, env, stdout, stderr) }()

	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	client := &http.Client{Transport: tr, Timeout: 200 * time.Millisecond}

	// Allowed poll: run binds the port asynchronously.
	deadline := time.Now().Add(waitTimeout)
	for {
		select {
		case code := <-done:
			t.Fatalf("run() exited with %d before /healthz answered 200; stdout %q, stderr %q",
				code, stdout.String(), stderr.String())
		default:
		}
		resp, err := client.Get("http://" + addr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			cancel()
			select {
			case <-done:
				t.Fatalf("/healthz did not answer 200 within %s (last error %v); stdout %q",
					waitTimeout, err, stdout.String())
			case <-time.After(10 * time.Second):
				t.Fatalf("/healthz did not answer 200 within %s (last error %v), and run did not return after cancel",
					waitTimeout, err)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case code = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after cancel")
	}
	return code, stdout, stderr
}

func TestRunExitCodes(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		env        map[string]string
		wantCode   int
		wantStdout string // exact; empty means no output
		wantStderr string // substring; empty means no output
	}{
		{
			name:       "version",
			args:       []string{"version"},
			wantCode:   0,
			wantStdout: "ops-copilot dev\n",
		},
		{
			name:       "unknown argument",
			args:       []string{"bogus"},
			wantCode:   2,
			wantStderr: "usage: ops-copilot",
		},
		{
			name:       "missing configuration",
			args:       nil,
			env:        map[string]string{},
			wantCode:   1,
			wantStderr: "PUBLIC_URL: is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keepDefaultLogger(t)
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tt.args, envOf(tt.env), &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("run() = %d, want %d (stderr %q)", code, tt.wantCode, stderr.String())
			}
			if stdout.String() != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.wantStdout)
			}
			if tt.wantStderr == "" {
				if stderr.Len() != 0 {
					t.Errorf("stderr = %q, want it empty", stderr.String())
				}
			} else if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}

	t.Run("serves until cancelled", func(t *testing.T) {
		keepDefaultLogger(t)
		env := envOf(map[string]string{
			"PUBLIC_URL":  "https://ops.example.test",
			"LISTEN_ADDR": freeAddr(t),
			"DATA_DIR":    t.TempDir(),
		})

		code, stdout, stderr := serveUntilHealthy(t, env)
		if code != 0 {
			t.Errorf("run() = %d, want 0 (stderr %q)", code, stderr.String())
		}

		lines := jsonLines(t, stdout)
		l, ok := findLine(lines, "config loaded")
		if !ok {
			t.Fatalf("no %q line in stdout: %q", "config loaded", stdout.String())
		}
		if l["public_url"] != "https://ops.example.test" {
			t.Errorf("config loaded public_url = %v, want %q", l["public_url"], "https://ops.example.test")
		}
		if l["version"] != "dev" {
			t.Errorf("config loaded version = %v, want %q", l["version"], "dev")
		}
		if _, ok := findLine(lines, "stopped"); !ok {
			t.Errorf("no %q line in stdout: %q", "stopped", stdout.String())
		}
	})
}

func TestRunListenError(t *testing.T) {
	keepDefaultLogger(t)
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = held.Close() })
	addr := held.Addr().String()

	dataDir := t.TempDir()
	env := envOf(map[string]string{
		"PUBLIC_URL":  "https://ops.example.test",
		"LISTEN_ADDR": addr,
		"DATA_DIR":    dataDir,
	})
	// The deadline only bounds a run that wrongly starts serving.
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	if code := run(ctx, nil, env, &stdout, &stderr); code != 1 {
		t.Fatalf("run() = %d, want 1 (stdout %q)", code, stdout.String())
	}

	lines := jsonLines(t, &stdout)
	var named bool
	for _, l := range lines {
		if l["level"] == "ERROR" && l["addr"] == addr {
			named = true
		}
	}
	if !named {
		t.Errorf("no error log line with addr %q in stdout: %q", addr, stdout.String())
	}

	// The store opened before the listen failed, so run must close it
	// cleanly: no close error, and the last connection's close checkpoints
	// and removes the WAL file.
	if _, ok := findLine(lines, "store opened"); !ok {
		t.Errorf("no %q line in stdout: %q", "store opened", stdout.String())
	}
	if _, ok := findLine(lines, "store close failed"); ok {
		t.Errorf("unexpected %q line in stdout: %q", "store close failed", stdout.String())
	}
	wal := filepath.Join(dataDir, sqlite.FileName+"-wal")
	if _, err := os.Stat(wal); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat %s: err = %v, want it not to exist after run returns", wal, err)
	}
}

func TestRunCreatesDatabase(t *testing.T) {
	keepDefaultLogger(t)
	dataDir := t.TempDir()
	env := envOf(map[string]string{
		"PUBLIC_URL":  "https://ops.example.test",
		"LISTEN_ADDR": freeAddr(t),
		"DATA_DIR":    dataDir,
	})

	code, stdout, stderr := serveUntilHealthy(t, env)
	if code != 0 {
		t.Errorf("run() = %d, want 0 (stderr %q)", code, stderr.String())
	}

	dbPath := filepath.Join(dataDir, sqlite.FileName)
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat %s: %v", dbPath, err)
	}
	if !info.Mode().IsRegular() {
		t.Errorf("%s mode = %v, want a regular file", dbPath, info.Mode())
	}

	lines := jsonLines(t, stdout)
	l, ok := findLine(lines, "store opened")
	if !ok {
		t.Fatalf("no %q line in stdout: %q", "store opened", stdout.String())
	}
	if l["path"] != dbPath {
		t.Errorf("store opened path = %v, want %q", l["path"], dbPath)
	}
	if _, ok := findLine(lines, "store close failed"); ok {
		t.Errorf("unexpected %q line in stdout: %q", "store close failed", stdout.String())
	}
	// A clean close checkpoints and removes the WAL file.
	wal := dbPath + "-wal"
	if _, err := os.Stat(wal); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat %s: err = %v, want it not to exist after run returns", wal, err)
	}
}

func TestRunRefusesNewerSchema(t *testing.T) {
	keepDefaultLogger(t)
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, sqlite.FileName)

	fixture, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	if _, err := fixture.ExecContext(context.Background(), "PRAGMA user_version = 99"); err != nil {
		_ = fixture.Close()
		t.Fatalf("set user_version: %v", err)
	}
	if err := fixture.Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}

	env := envOf(map[string]string{
		"PUBLIC_URL":  "https://ops.example.test",
		"LISTEN_ADDR": freeAddr(t),
		"DATA_DIR":    dataDir,
	})
	// The deadline only bounds a run that wrongly starts serving.
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	if code := run(ctx, nil, env, &stdout, &stderr); code != 1 {
		t.Fatalf("run() = %d, want 1 (stdout %q)", code, stdout.String())
	}

	lines := jsonLines(t, &stdout)
	l, ok := findLine(lines, "store open failed")
	if !ok {
		t.Fatalf("no %q line in stdout: %q", "store open failed", stdout.String())
	}
	if l["level"] != "ERROR" {
		t.Errorf("store open failed level = %v, want ERROR", l["level"])
	}
	if msg, _ := l["error"].(string); !strings.Contains(msg, "newer than this binary") {
		t.Errorf("store open failed error = %v, want it to contain %q", l["error"], "newer than this binary")
	}
	if _, ok := findLine(lines, "listening"); ok {
		t.Errorf("unexpected %q line: run must not listen when the store fails to open", "listening")
	}
}
