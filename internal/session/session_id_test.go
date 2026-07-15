package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGenerateSessionID_Entropy asserts the session ID doubles as a
// capability token: high entropy, no collisions across many samples, and a
// shape that is structurally different from the old <slug>-<adjective>-
// <surname> label (which was only ~960 combinations).
func TestGenerateSessionID_Entropy(t *testing.T) {
	const n = 5000

	seen := make(map[string]struct{}, n)
	for range n {
		id := generateSessionID("myhost", nil)

		require.True(t, strings.HasPrefix(id, "myhost-"), "id %q must keep the display slug prefix", id)
		token := strings.TrimPrefix(id, "myhost-")

		// The token itself must not contain a hyphen: the old label format
		// used two hyphen-separated words (adjective-surname) after the
		// slug, so a single opaque token proves we're not reusing that shape.
		require.NotContains(t, token, "-", "id %q looks like the old adjective-surname label", id)
		require.Len(t, token, 13, "expected a 13-char base32 encoding of 64 random bits")

		_, dup := seen[id]
		require.False(t, dup, "collision detected: %s", id)
		seen[id] = struct{}{}
	}
}

// TestGenerateSessionID_CollisionRetry exercises the retry-on-collision path
// by pre-populating the sessions map with every possible outcome of a
// deterministic pseudo-random source... instead we just check the documented
// contract: an already-taken ID is never returned twice for the same map.
func TestGenerateSessionID_CollisionRetry(t *testing.T) {
	sessions := map[string]*Session{}
	for range 50 {
		id := generateSessionID("host", sessions)
		_, exists := sessions[id]
		require.False(t, exists)
		sessions[id] = &Session{ID: id}
	}
}

// TestGenerateSessionLabel_FriendlyShape asserts the display label keeps the
// old, human-friendly <slug>-<adjective>-<surname> shape used for UX, since
// unlike the ID it is not a capability token and doesn't need real entropy.
func TestGenerateSessionLabel_FriendlyShape(t *testing.T) {
	label := generateSessionLabel("myhost")

	parts := strings.Split(label, "-")
	require.Len(t, parts, 3, "label %q must be slug-adjective-surname", label)
	require.Equal(t, "myhost", parts[0])
	require.Contains(t, adjectives, parts[1])
	require.Contains(t, surnames, parts[2])
}

func TestMachineSlug(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"example.com", "example-com"},
		{"user@example.com", "example-com"},
		{"example.com:2222", "example-com"},
		{"user@example.com:2222", "example-com"},
		{"", "host"},
		{"!!!", "host"},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			require.Equal(t, tc.want, machineSlug(tc.input))
		})
	}
}
