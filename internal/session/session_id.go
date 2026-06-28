package session

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
	"time"
)

func generateSessionID(machine string, sessions map[string]*Session) string {
	slug := machineSlug(machine)
	for range 100 {
		id := fmt.Sprintf("%s-%s-%s", slug, randomAdjective(), randomSurname())
		if _, ok := sessions[id]; !ok {
			return id
		}
	}
	return fmt.Sprintf("%s-%d", slug, time.Now().UnixNano())
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
