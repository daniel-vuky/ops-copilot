package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
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
		addr := freeAddr(t)
		env := envOf(map[string]string{
			"PUBLIC_URL":  "https://ops.example.test",
			"LISTEN_ADDR": addr,
		})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var stdout, stderr bytes.Buffer
		done := make(chan int, 1)
		go func() { done <- run(ctx, nil, env, &stdout, &stderr) }()

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
				t.Fatalf("/healthz did not answer 200 within %s (last error %v)", waitTimeout, err)
			}
			time.Sleep(10 * time.Millisecond)
		}

		cancel()
		select {
		case code := <-done:
			if code != 0 {
				t.Errorf("run() = %d, want 0 (stderr %q)", code, stderr.String())
			}
		case <-time.After(10 * time.Second):
			t.Fatal("run did not return after cancel")
		}

		lines := jsonLines(t, &stdout)
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

	env := envOf(map[string]string{
		"PUBLIC_URL":  "https://ops.example.test",
		"LISTEN_ADDR": addr,
	})
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), nil, env, &stdout, &stderr); code != 1 {
		t.Fatalf("run() = %d, want 1 (stdout %q)", code, stdout.String())
	}

	var named bool
	for _, l := range jsonLines(t, &stdout) {
		if l["level"] == "ERROR" && l["addr"] == addr {
			named = true
		}
	}
	if !named {
		t.Errorf("no error log line with addr %q in stdout: %q", addr, stdout.String())
	}
}
