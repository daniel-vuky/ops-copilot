// Package logging builds the JSON logger used across the binary and carries a
// request-scoped logger and request id through a context.
package logging

import (
	"io"
	"log/slog"
	"time"
)

// New returns a logger that writes one JSON object per record to w and drops
// records below level. The time field is always rendered in UTC as RFC 3339
// with nanoseconds, whatever the record's zone.
func New(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: utcTime,
	}))
}

// utcTime renders the built-in time attribute in UTC.
func utcTime(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
		a.Value = slog.StringValue(a.Value.Time().UTC().Format(time.RFC3339Nano))
	}
	return a
}
