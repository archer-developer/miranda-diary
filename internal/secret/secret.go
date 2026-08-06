// Package secret provides the String type, a string wrapper that redacts its
// value in all logging and marshaling sinks. Use it to hold sensitive values
// (encryption keys, tokens) so they cannot accidentally leak through
// fmt.Printf, slog, or JSON encoding.
package secret

import "log/slog"

// String holds a sensitive value that must not appear in logs or marshaled
// output. Wrap a sensitive string immediately after reading it from a tool
// call or environment variable — never hold the raw string any longer than
// necessary before wrapping.
type String struct{ value string }

// New wraps raw in a String.
func New(raw string) String { return String{raw} }

// Value returns the raw secret. Call only when the value must actually be
// used (e.g. passed to a crypto function), never to produce log output.
func (s String) Value() string { return s.value }

// String implements fmt.Stringer — fmt.Printf("%v", s) yields "[REDACTED]".
func (s String) String() string { return "[REDACTED]" }

// MarshalText implements encoding.TextMarshaler.
func (s String) MarshalText() ([]byte, error) { return []byte("[REDACTED]"), nil }

// MarshalJSON implements json.Marshaler.
func (s String) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }

// LogValue implements slog.LogValuer — slog.Any("k", s) logs "[REDACTED]".
func (s String) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }
