package llm

import "fmt"

// Secret is an API key. It prints as [redacted] under every fmt verb and in
// JSON. Hold it behind a pointer or in an exported field: a bare Secret in an
// unexported struct field is printed in clear by fmt.
type Secret string

// Reveal returns the value; use it only to build the request header.
func (s Secret) Reveal() string { return string(s) }

// Format implements fmt.Formatter.
func (Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte("[redacted]")) }

// MarshalJSON implements json.Marshaler.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }
