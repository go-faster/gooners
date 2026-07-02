// Package gateway implements an MCP gateway that proxies multiple upstream MCP servers.
package gateway

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRedactor_BuiltinLabelValue(t *testing.T) {
	r, _ := NewRedactor(nil, 0)
	out := r.Redact("password=hunter2 token:abc api_key=xyz")
	require.Contains(t, out, "password=[REDACTED]")
	require.Contains(t, out, "token:")
}

func TestRedactor_Custom(t *testing.T) {
	r, _ := NewRedactor([]string{`AKIAFOO`}, 0)
	out := r.Redact("AKIAFOO1234567890ABCD")
	require.Contains(t, out, "[REDACTED]")
}

func TestRedactor_EntropyOffByDefault(t *testing.T) {
	r, _ := NewRedactor(nil, 0)
	s := "not-a-secret-just-text-123" // no builtin match, entropy should be ignored
	require.Equal(t, s, r.Redact(s))
}

func TestRedactor_NoFalsePositiveShort(t *testing.T) {
	r, _ := NewRedactor(nil, 4.5)
	require.Equal(t, "short", r.Redact("short"))
}

func TestRedactor_Idempotent(t *testing.T) {
	r, _ := NewRedactor(nil, 0)
	s := "password=[REDACTED]"
	require.Equal(t, s, r.Redact(s))
}
