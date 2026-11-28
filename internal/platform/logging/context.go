package logging

import (
	"context"
	"log/slog"
)

// ctxKey is the type of this package's context keys, so they cannot collide
// with keys from other packages.
type ctxKey int

const (
	loggerKey ctxKey = iota
	requestIDKey
)

// WithLogger returns a copy of ctx that carries l.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, l)
}

// FromContext returns the logger carried by ctx, or slog.Default() when ctx
// carries none. It never returns nil.
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// WithRequestID returns a copy of ctx that carries the request id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID returns the request id carried by ctx, or "" when there is none.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}
