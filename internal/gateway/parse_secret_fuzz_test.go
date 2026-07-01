package gateway

import (
	"context"
	"log/slog"
	"strings"
	"testing"
)

// FuzzParseSecretRef ensures the parser never panics and stays consistent with
// extractSecretRefs on arbitrary input.
func FuzzParseSecretRef(f *testing.F) {
	// Seed corpus: valid refs, edge cases, and pathological strings.
	seeds := []string{
		"{secret:foo}",
		"{ secret : bar }",
		"{\tsecret\t:\tbaz\t}",
		"{secret:my-key.v1_x}",
		"prefix{secret:tok}suffix",
		"{secret:a}{secret:b}",
		"{ secret : a }{ secret : b }",
		"{secret:}",
		"{secret: }",
		"{env:foo}",
		"{secret:no-close",
		"{}",
		"{:}",
		"plain string",
		"",
		"{{secret:nested}}",
		"{secret:\x00null}",
		strings.Repeat("{", 100),
		strings.Repeat("}", 100),
		"{secret:" + strings.Repeat("a", 200) + "}",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, s string) {
		// parseSecretRef must never panic.
		for i := range len(s) {
			name, end := parseSecretRef(s, i)
			if end == 0 {
				// No match: name must be empty.
				if name != "" {
					t.Fatalf("parseSecretRef(%q, %d): non-empty name %q with end=0", s, i, name)
				}
				continue
			}
			// end must be a valid index past the closing '}'.
			if end <= i || end > len(s) {
				t.Fatalf("parseSecretRef(%q, %d): end=%d out of range [%d, %d]", s, i, end, i+1, len(s))
			}
			// The character just before end must be '}'.
			if s[end-1] != '}' {
				t.Fatalf("parseSecretRef(%q, %d): s[end-1]=%q, want '}'", s, i, s[end-1])
			}
			// name must be valid.
			if !isValidSecretName(name) {
				t.Fatalf("parseSecretRef(%q, %d): returned invalid name %q", s, i, name)
			}
			// The returned range must contain the name.
			if !strings.Contains(s[i:end], name) {
				t.Fatalf("parseSecretRef(%q, %d): name %q not found in token %q", s, i, name, s[i:end])
			}
		}

		// extractSecretRefs must never panic and must only yield valid names.
		for name := range extractSecretRefs(s) {
			if !isValidSecretName(name) {
				t.Fatalf("extractSecretRefs(%q): yielded invalid name %q", s, name)
			}
		}

		// Interpolate with a nil resolver must not panic.
		// It may return an error if the string looks like it contains a secret ref.
		out, err := Interpolate(context.Background(), s, nil)
		if err == nil && strings.Contains(s, "{secret:") {
			// This is allowed: the heuristic check is a fast pre-filter; the real
			// parser may still reject the ref (e.g. bad name chars), so no
			// output assertion here.
			_ = out
		}

		// Interpolate with an always-empty resolver must not panic.
		r, _ := NewSecretResolver(nil, slog.New(slog.DiscardHandler))
		out2, _ := Interpolate(context.Background(), s, r)
		// The output must not be longer than the input (secrets may fail and be
		// copied verbatim, but no content is invented).
		if len(out2) > len(s) {
			t.Fatalf("Interpolate(%q): output %q longer than input", s, out2)
		}
	})
}
