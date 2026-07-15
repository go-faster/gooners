package session

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// generateSessionID returns an opaque session ID: a display slug plus 64 bits
// of crypto/rand. It doubles as a capability token (the isolation boundary
// for sandboxes), so unlike [generateSessionLabel] it must be hard to guess.
// The signature (and the sessions map argument) is kept stable so callers
// don't need to change: the map is only consulted to retry on the
// astronomically unlikely event of a collision.
func generateSessionID(machine string, sessions map[string]*Session) string {
	slug := machineSlug(machine)
	for range 100 {
		id := slug + "-" + randomToken()
		if _, ok := sessions[id]; !ok {
			return id
		}
	}
	// Unreachable outside an astronomically unlikely run of collisions. The
	// fallback must still be an unguessable token, never a predictable value
	// like a timestamp, since the ID is a capability token.
	return slug + "-" + randomToken() + randomToken()
}

// generateSessionLabel returns a friendly, display-only name for machine.
// Unlike the session ID, it is not a capability token: it is not unique and
// must never be used to authorize access to a session.
func generateSessionLabel(machine string) string {
	return fmt.Sprintf("%s-%s-%s", machineSlug(machine), randomAdjective(), randomSurname())
}

var tokenEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// randomToken returns 64 bits of crypto/rand encoded as lowercase, unpadded base32.
func randomToken() string {
	var b [8]byte
	// crypto/rand.Read never returns an error on supported platforms.
	_, _ = rand.Read(b[:])
	return strings.ToLower(tokenEncoding.EncodeToString(b[:]))
}

var adjectives = []string{
	"cool", "silly", "brave", "happy", "clever", "eager", "funny", "gentle",
	"jolly", "kind", "lively", "nice", "proud", "quiet", "witty", "young",
	"zany", "fancy", "mighty", "swift", "calm", "bold", "wise", "merry",
	"plucky", "spry", "zesty", "quirky", "jovial", "vibrant",
}

var surnames = []string{
	"einstein", "newton", "darwin", "curie", "tesla", "hopper", "lovelace",
	"turing", "galileo", "kepler", "pasteur", "nobel", "bohr", "fermi",
	"feynman", "hawking", "torvalds", "knuth", "dijkstra", "musk", "neumann",
	"oppenheimer", "shannon", "babbage", "ellis", "carver", "cerf", "kahn", "ritchie",
	"pike", "postel", "keller",
}

func machineSlug(m string) string {
	// strip user@ prefix and :port suffix
	if at := strings.LastIndex(m, "@"); at != -1 {
		m = m[at+1:]
	}
	if idx := strings.LastIndex(m, ":"); idx != -1 {
		m = m[:idx]
	}
	m = strings.ToLower(strings.TrimSpace(m))

	var b strings.Builder
	for _, r := range m {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else if b.Len() > 0 {
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "host"
	}
	return s
}

func randomAdjective() string {
	return adjectives[randomIndex(len(adjectives))]
}

func randomSurname() string {
	return surnames[randomIndex(len(surnames))]
}

func randomIndex(n int) int {
	if n <= 0 {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return int(time.Now().UnixNano() % int64(n))
	}
	return int(v.Int64())
}
