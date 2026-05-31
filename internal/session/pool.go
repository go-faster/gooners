package session

import (
	"crypto/rand"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

type Session struct {
	ID        string
	Machine   string
	CreatedAt time.Time
	client    *ssh.Client
}

type SessionInfo struct {
	ID        string    `json:"id"`
	Machine   string    `json:"machine"`
	CreatedAt time.Time `json:"created_at"`
}

type Provider interface {
	Get(id string) (*ssh.Client, error)
}

type Pool struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

func NewPool() *Pool {
	return &Pool{sessions: make(map[string]*Session)}
}

func (p *Pool) Open(machine string) (string, error) {
	return p.OpenCfg(Config{Machine: machine})
}

func (p *Pool) OpenCfg(cfg Config) (id string, _ error) {
	client, err := cfg.dial()
	if err != nil {
		slog.Debug("ssh dial failed", "machine", cfg.Machine, "err", err)
		return "", err
	}
	defer func() {
		slog.Debug("ssh session opened", "id", id, "machine", cfg.Machine)
	}()

	slug := machineSlug(cfg.Machine)
	p.mu.Lock()
	defer p.mu.Unlock()

	for range 100 {
		id = fmt.Sprintf("%s-%s-%s", slug, randomAdjective(), randomSurname())
		if _, ok := p.sessions[id]; !ok {
			sess := &Session{
				ID:        id,
				Machine:   cfg.Machine,
				CreatedAt: time.Now(),
				client:    client,
			}
			p.sessions[id] = sess

			return id, nil
		}
	}

	// Fallback (extremely unlikely): append nanos
	id = fmt.Sprintf("%s-%d", slug, time.Now().UnixNano())
	sess := &Session{
		ID:        id,
		Machine:   cfg.Machine,
		CreatedAt: time.Now(),
		client:    client,
	}
	p.sessions[id] = sess
	return id, nil
}

func (p *Pool) Close(id string) error {
	p.mu.Lock()
	s, ok := p.sessions[id]
	if ok {
		_ = s.client.Close()
		delete(p.sessions, id)
		slog.Debug("ssh session closed", "id", id, "machine", s.Machine)
	}
	p.mu.Unlock()
	return nil
}

func (p *Pool) Get(id string) (*ssh.Client, error) {
	p.mu.RLock()
	s, ok := p.sessions[id]
	p.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("session not found: %s", id)
	}
	return s.client, nil
}

func (p *Pool) List() []SessionInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]SessionInfo, 0, len(p.sessions))
	for _, s := range p.sessions {
		out = append(out, SessionInfo{ID: s.ID, Machine: s.Machine, CreatedAt: s.CreatedAt})
	}
	return out
}

func (p *Pool) Shutdown() {
	p.mu.Lock()
	for _, s := range p.sessions {
		_ = s.client.Close()
		slog.Debug("ssh session closed (shutdown)", "id", s.ID, "machine", s.Machine)
	}
	p.sessions = make(map[string]*Session)
	p.mu.Unlock()
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
