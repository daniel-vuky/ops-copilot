package config

import "log/slog"

// redacted is what every formatting path prints for a Secret.
const redacted = "[redacted]"

// Secret holds a sensitive string, such as a token or password, and prints
// as "[redacted]" through fmt, slog, encoding/json and encoding.TextMarshaler.
// Only Reveal returns the value.
//
// The value sits behind an unexported pointer, so when fmt reaches a Secret
// by reflection (for example as an unexported struct field, where it cannot
// call String) it prints an address, not the value.
type Secret struct {
	v *string
}

// NewSecret wraps v.
func NewSecret(v string) Secret {
	return Secret{v: &v}
}

// Reveal returns the wrapped value, or "" for the zero Secret. Call it only
// where the value is needed, never to log it.
func (s Secret) Reveal() string {
	if s.v == nil {
		return ""
	}
	return *s.v
}

// String returns "[redacted]".
func (Secret) String() string { return redacted }

// GoString returns "[redacted]", so %#v does not print the value.
func (Secret) GoString() string { return redacted }

// LogValue returns "[redacted]" for slog.
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalText returns "[redacted]", which encoding/json and other text
// encoders use.
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }
