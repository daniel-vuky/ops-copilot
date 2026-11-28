package config_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/daniel-vuky/ops-copilot/internal/platform/config"
)

func TestSecretIsRedacted(t *testing.T) {
	t.Parallel()

	const raw = "hunter2"
	s := config.NewSecret(raw)

	slogLine := func(attr slog.Attr) string {
		var buf bytes.Buffer
		slog.New(slog.NewJSONHandler(&buf, nil)).LogAttrs(t.Context(), slog.LevelInfo, "msg", attr)
		return buf.String()
	}
	jsonOut := func() string {
		b, err := json.Marshal(struct{ Token config.Secret }{s})
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		return string(b)
	}

	// fmt cannot call String on an unexported field, so it prints the
	// struct by reflection: the pointer's address ("0x"), not the value.
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"fmt verbs", fmt.Sprintf("%v %s %+v %#v", s, s, s, s), "[redacted]"},
		{"slog value", slogLine(slog.Any("k", s)), "[redacted]"},
		{"slog struct", slogLine(slog.Any("cfg", struct{ Token config.Secret }{s})), "[redacted]"},
		{"json struct", jsonOut(), "[redacted]"},
		{"fmt unexported field", fmt.Sprintf("%+v", struct{ token config.Secret }{s}), "0x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if strings.Contains(tt.got, raw) {
				t.Errorf("output %q contains the secret", tt.got)
			}
			if !strings.Contains(tt.got, tt.want) {
				t.Errorf("output %q does not contain %q", tt.got, tt.want)
			}
		})
	}

	if got := s.Reveal(); got != raw {
		t.Errorf("Reveal() = %q, want %q", got, raw)
	}
}

func TestSecretZeroValue(t *testing.T) {
	t.Parallel()

	var s config.Secret
	if got := s.Reveal(); got != "" {
		t.Errorf("zero Reveal() = %q, want empty", got)
	}
	if got := s.String(); got != "[redacted]" {
		t.Errorf("zero String() = %q, want [redacted]", got)
	}
}
