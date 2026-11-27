package config_test

import (
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"github.com/daniel-vuky/ops-copilot/internal/platform/config"
)

const (
	validURL        = "https://ops.example.test"
	publicURLReason = "must be an https origin: https://host[:port] with no path, query, fragment or user info"
)

// env returns a getenv function backed by m.
func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// withURL returns m plus a valid PUBLIC_URL unless m already sets one.
func withURL(m map[string]string) map[string]string {
	out := map[string]string{"PUBLIC_URL": validURL}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// varError returns the *VarError for name inside err, or nil.
func varError(err error, name string) *config.VarError {
	var joined interface{ Unwrap() []error }
	if errors.As(err, &joined) {
		for _, e := range joined.Unwrap() {
			if ve := varError(e, name); ve != nil {
				return ve
			}
		}
		return nil
	}
	var ve *config.VarError
	if errors.As(err, &ve) && ve.Name == name {
		return ve
	}
	return nil
}

func TestLoadDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(env(map[string]string{"PUBLIC_URL": validURL}))
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if cfg.PublicURL != validURL {
		t.Errorf("PublicURL = %q, want %q", cfg.PublicURL, validURL)
	}
	if cfg.DisplayTZ != "Asia/Ho_Chi_Minh" {
		t.Errorf("DisplayTZ = %q, want Asia/Ho_Chi_Minh", cfg.DisplayTZ)
	}
	if cfg.DisplayLocation == nil || cfg.DisplayLocation.String() != "Asia/Ho_Chi_Minh" {
		t.Errorf("DisplayLocation = %v, want Asia/Ho_Chi_Minh", cfg.DisplayLocation)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want :8080", cfg.ListenAddr)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want %v", cfg.LogLevel, slog.LevelInfo)
	}
}

func TestLoadPublicURLAccepted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in, want string
	}{
		{"https://ops.example.test", "https://ops.example.test"},
		{"https://ops.example.test/", "https://ops.example.test"},
		{"https://ops.example.test:8443", "https://ops.example.test:8443"},
		{"HTTPS://Ops.Example.Test", "https://ops.example.test"},
		{"https://ops.example.test:443", "https://ops.example.test"},
		{"https://ops.example.test:443/", "https://ops.example.test"},
		{"https://xn--fiq228c.test", "https://xn--fiq228c.test"},
		{"https://[::1]:8443", "https://[::1]:8443"},
		{"https://[::1]:443", "https://[::1]"},
		{"https://192.0.2.10", "https://192.0.2.10"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Load(env(map[string]string{"PUBLIC_URL": tt.in}))
			if err != nil {
				t.Fatalf("Load: unexpected error: %v", err)
			}
			if cfg.PublicURL != tt.want {
				t.Errorf("PublicURL = %q, want %q", cfg.PublicURL, tt.want)
			}
		})
	}
}

func TestLoadPublicURLRejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, in string
	}{
		{"missing", ""},
		{"http scheme", "http://ops.example.test"},
		{"path", "https://ops.example.test/app"},
		{"user info", "https://u@ops.example.test"},
		{"query", "https://ops.example.test?x=1"},
		{"empty query", "https://ops.example.test?"},
		{"fragment", "https://ops.example.test#f"},
		{"empty fragment", "https://ops.example.test#"},
		{"no host with port", "https://:8443"},
		{"trailing colon", "https://ops.example.test:"},
		{"port out of range", "https://ops.example.test:99999"},
		{"port zero", "https://ops.example.test:0"},
		{"no scheme", "ops.example.test"},
		{"no host", "https://"},
		{"opaque", "https:ops.example.test"},
		{"port leading zero", "https://ops.example.test:0443"},
		{"quote in host", `https://ops.example.test"onload=x`},
		{"angle bracket in host", "https://ops.example.test<b>"},
		{"comma in host", "https://a.test,b.test"},
		{"escaped percent in host", "https://a%25b"},
		{"non-ASCII host", "https://中.test"},
		{"IPv6 zone", "https://[fe80::1%25en0]"},
		{"trailing dot", "https://ops.example.test."},
		{"leading hyphen", "https://-ops.example.test"},
		{"bracketed name", "https://[ops.example.test]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Load(env(map[string]string{"PUBLIC_URL": tt.in}))
			ve := varError(err, "PUBLIC_URL")
			if ve == nil {
				t.Fatalf("Load(%q): want a PUBLIC_URL *VarError, got %v", tt.in, err)
			}
			want := "PUBLIC_URL: " + publicURLReason
			if tt.in == "" {
				want = "PUBLIC_URL: is required"
			}
			if err.Error() != want {
				t.Errorf("error = %q, want the fixed phrase %q", err, want)
			}
		})
	}
}

func TestLoadPublicURLErrorHidesPassword(t *testing.T) {
	t.Parallel()

	// Built from parts: a literal URL with user info trips gosec G101.
	raw := (&url.URL{Scheme: "https", User: url.UserPassword("u", "pw"), Host: "ops.example.test"}).String()
	_, err := config.Load(env(map[string]string{"PUBLIC_URL": raw}))
	if err == nil {
		t.Fatal("Load: want an error for user info, got nil")
	}
	if strings.Contains(err.Error(), "pw") {
		t.Errorf("error %q contains the password", err)
	}
	if varError(err, "PUBLIC_URL") == nil {
		t.Errorf("want a PUBLIC_URL *VarError, got %v", err)
	}
}

func TestLoadDisplayTZ(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		ok      bool
		wantErr string
	}{
		{"UTC", true, ""},
		{"Europe/Paris", true, ""},
		{"Mars/Olympus", false, "DISPLAY_TZ: unknown time zone"},
		{"../../etc/passwd", false, "DISPLAY_TZ: unknown time zone"},
		{"Local", false, "DISPLAY_TZ: must name a zone, not Local"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Load(env(withURL(map[string]string{"DISPLAY_TZ": tt.in})))
			if !tt.ok {
				if varError(err, "DISPLAY_TZ") == nil {
					t.Fatalf("Load: want a DISPLAY_TZ *VarError, got %v", err)
				}
				if err.Error() != tt.wantErr {
					t.Errorf("error = %q, want the fixed phrase %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: unexpected error: %v", err)
			}
			if cfg.DisplayTZ != tt.in || cfg.DisplayLocation.String() != tt.in {
				t.Errorf("DisplayTZ = %q, DisplayLocation = %v, want %q", cfg.DisplayTZ, cfg.DisplayLocation, tt.in)
			}
		})
	}
}

func TestLoadListenAddr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in string
		ok bool
	}{
		{":9000", true},
		{"127.0.0.1:8080", true},
		{"[::1]:8080", true},
		{"8080", false},
		{":+80", false},
		{":0", false},
		{":70000", false},
		{":http", false},
		{":", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Load(env(withURL(map[string]string{"LISTEN_ADDR": tt.in})))
			if !tt.ok {
				if varError(err, "LISTEN_ADDR") == nil {
					t.Fatalf("Load: want a LISTEN_ADDR *VarError, got %v", err)
				}
				if want := "LISTEN_ADDR: must be host:port with a port from 1 to 65535"; err.Error() != want {
					t.Errorf("error = %q, want the fixed phrase %q", err, want)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: unexpected error: %v", err)
			}
			if cfg.ListenAddr != tt.in {
				t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, tt.in)
			}
		})
	}
}

func TestLoadLogLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		ok   bool
		want slog.Level
	}{
		{"DEBUG", true, slog.LevelDebug},
		{"info", true, slog.LevelInfo},
		{"warn", true, slog.LevelWarn},
		{"Error", true, slog.LevelError},
		{"verbose", false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Load(env(withURL(map[string]string{"LOG_LEVEL": tt.in})))
			if !tt.ok {
				if varError(err, "LOG_LEVEL") == nil {
					t.Fatalf("Load: want a LOG_LEVEL *VarError, got %v", err)
				}
				if want := "LOG_LEVEL: must be one of debug, info, warn, error"; err.Error() != want {
					t.Errorf("error = %q, want the fixed phrase %q", err, want)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: unexpected error: %v", err)
			}
			if cfg.LogLevel != tt.want {
				t.Errorf("LogLevel = %v, want %v", cfg.LogLevel, tt.want)
			}
		})
	}
}

func TestLoadJoinsAllErrorsInOrder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		env      map[string]string
		prefixes []string
	}{
		{
			name:     "three bad",
			env:      map[string]string{"DISPLAY_TZ": "Nope/Zone", "LOG_LEVEL": "loud"},
			prefixes: []string{"PUBLIC_URL: ", "DISPLAY_TZ: ", "LOG_LEVEL: "},
		},
		{
			name: "all four bad",
			env: map[string]string{
				"PUBLIC_URL":  "http://ops.example.test",
				"DISPLAY_TZ":  "Nope/Zone",
				"LISTEN_ADDR": "nowhere",
				"LOG_LEVEL":   "loud",
			},
			prefixes: []string{"PUBLIC_URL: ", "DISPLAY_TZ: ", "LISTEN_ADDR: ", "LOG_LEVEL: "},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Load(env(tt.env))
			if err == nil {
				t.Fatal("Load: want an error, got nil")
			}
			lines := strings.Split(err.Error(), "\n")
			if len(lines) != len(tt.prefixes) {
				t.Fatalf("error has %d lines, want %d: %q", len(lines), len(tt.prefixes), err)
			}
			for i, p := range tt.prefixes {
				if !strings.HasPrefix(lines[i], p) {
					t.Errorf("line %d = %q, want prefix %q", i, lines[i], p)
				}
			}
			for _, raw := range tt.env {
				if strings.Contains(err.Error(), raw) {
					t.Errorf("error %q echoes raw value %q", err, raw)
				}
			}
		})
	}
}

func TestVarErrorMessage(t *testing.T) {
	t.Parallel()

	e := &config.VarError{Name: "LOG_LEVEL", Reason: "is required"}
	if got, want := e.Error(), "LOG_LEVEL: is required"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestLogAttrs(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(env(map[string]string{"PUBLIC_URL": validURL, "LOG_LEVEL": "warn"}))
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	got := map[string]string{}
	var keys []string
	for _, a := range cfg.LogAttrs() {
		keys = append(keys, a.Key)
		got[a.Key] = a.Value.String()
	}
	if want := []string{"public_url", "display_tz", "listen_addr", "log_level"}; strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("keys = %v, want %v", keys, want)
	}
	want := map[string]string{
		"public_url":  validURL,
		"display_tz":  "Asia/Ho_Chi_Minh",
		"listen_addr": ":8080",
		"log_level":   "WARN",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}
