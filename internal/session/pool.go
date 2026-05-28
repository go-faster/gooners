package session

import (
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
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

func (p *Pool) OpenCfg(cfg Config) (string, error) {
	cc, addr, err := cfg.clientConfig()
	if err != nil {
		return "", err
	}
	client, err := ssh.Dial("tcp", addr, cc)
	if err != nil {
		return "", err
	}
	id := uuid.New().String()
	sess := &Session{
		ID:        id,
		Machine:   cfg.Machine,
		CreatedAt: time.Now(),
		client:    client,
	}
	p.mu.Lock()
	p.sessions[id] = sess
	p.mu.Unlock()
	return id, nil
}

func (p *Pool) Close(id string) error {
	p.mu.Lock()
	s, ok := p.sessions[id]
	if ok {
		//nolint:errcheck // Close error not actionable during session cleanup
		s.client.Close()
		delete(p.sessions, id)
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
		//nolint:errcheck // Close error not actionable during shutdown
		s.client.Close()
	}
	p.sessions = make(map[string]*Session)
	p.mu.Unlock()
}
