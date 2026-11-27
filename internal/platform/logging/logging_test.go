package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/daniel-vuky/ops-copilot/internal/platform/logging"
)

func TestNewDropsRecordsBelowLevel(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := logging.New(&buf, slog.LevelWarn)
	logger.Info("dropped")
	if buf.Len() != 0 {
		t.Fatalf("info record at warn level was written: %q", buf.String())
	}

	logger.Warn("kept")
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("output is not one JSON object: %v: %q", err, buf.String())
	}
	if line["msg"] != "kept" || line["level"] != "WARN" {
		t.Errorf("line = %v, want msg kept at level WARN", line)
	}
}

func TestNewRendersTimeInUTC(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		when time.Time
		want string
	}{
		{
			name: "offset zone",
			when: time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("X", 7*3600)),
			want: `"time":"2026-01-01T20:04:05Z"`,
		},
		{
			name: "nanoseconds kept",
			when: time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.FixedZone("Y", -5*3600)),
			want: `"time":"2026-01-02T08:04:05.123456789Z"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			logger := logging.New(&buf, slog.LevelDebug)
			rec := slog.NewRecord(tt.when, slog.LevelWarn, "m", 0)
			if err := logger.Handler().Handle(context.Background(), rec); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if !strings.Contains(buf.String(), tt.want) {
				t.Errorf("output %q does not contain %s", buf.String(), tt.want)
			}
		})
	}
}

func TestFromContextFallsBackToDefault(t *testing.T) {
	t.Parallel()

	if got := logging.FromContext(context.Background()); got != slog.Default() {
		t.Errorf("FromContext(Background) = %p, want slog.Default() %p", got, slog.Default())
	}
	ctx := logging.WithLogger(context.Background(), nil)
	if got := logging.FromContext(ctx); got != slog.Default() {
		t.Errorf("FromContext after WithLogger(nil) = %p, want slog.Default()", got)
	}
}

func TestWithLoggerRoundTrip(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	l := logging.New(&buf, slog.LevelInfo)
	ctx := logging.WithLogger(context.Background(), l)
	if got := logging.FromContext(ctx); got != l {
		t.Errorf("FromContext = %p, want %p", got, l)
	}
}

func TestRequestID(t *testing.T) {
	t.Parallel()

	if got := logging.RequestID(context.Background()); got != "" {
		t.Errorf("RequestID(Background) = %q, want empty", got)
	}
	ctx := logging.WithRequestID(context.Background(), "0123abcd")
	if got := logging.RequestID(ctx); got != "0123abcd" {
		t.Errorf("RequestID = %q, want %q", got, "0123abcd")
	}
}
