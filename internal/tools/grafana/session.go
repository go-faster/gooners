package grafana

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// SessionManager manages active dashboard builder sessions.
type SessionManager struct {
	mu       sync.Mutex
	sessions map[string]*DashboardSession
	dir      string
	OnEvict  func(id string)
}

func NewSessionManager(dir string) *SessionManager {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create session dir %q: %v\n", dir, err)
	}
	m := &SessionManager{
		sessions: make(map[string]*DashboardSession),
		dir:      dir,
	}
	m.loadAll()
	return m
}

func (m *SessionManager) clone(s *DashboardSession) *DashboardSession {
	if s == nil {
		return nil
	}
	data, err := json.Marshal(s)
	if err != nil {
		// DashboardSession contains only JSON-serializable types; this is a
		// programming error if it ever fires.
		panic(fmt.Sprintf("grafana: session marshal failed: %v", err))
	}
	var res DashboardSession
	if err := json.Unmarshal(data, &res); err != nil {
		panic(fmt.Sprintf("grafana: session unmarshal failed: %v", err))
	}
	return &res
}

func (m *SessionManager) Add(s *DashboardSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cloned := m.clone(s)
	m.sessions[cloned.DashboardID] = cloned
	m.save(cloned)
}

func (m *SessionManager) Get(id string) (*DashboardSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session not found: %s", id)
	}
	s.TouchedAt = time.Now()
	m.save(s)
	return m.clone(s), nil
}

func (m *SessionManager) Update(id string, fn func(*DashboardSession) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return fmt.Errorf("session not found: %s", id)
	}
	cloned := m.clone(s)
	if err := fn(cloned); err != nil {
		return err
	}
	cloned.TouchedAt = time.Now()
	m.sessions[id] = cloned
	m.save(cloned)
	return nil
}

func (m *SessionManager) List() []*DashboardSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := make([]*DashboardSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		res = append(res, m.clone(s))
	}
	sort.Slice(res, func(i, j int) bool {
		return res[i].TouchedAt.After(res[j].TouchedAt)
	})
	return res
}

func (m *SessionManager) Delete(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
	_ = os.Remove(filepath.Join(m.dir, id+".json"))
}

func (m *SessionManager) save(s *DashboardSession) {
	data, err := json.Marshal(s)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(m.dir, s.DashboardID+".json"), data, 0o600)
}

func (m *SessionManager) loadAll() {
	files, err := os.ReadDir(m.dir)
	if err != nil {
		return
	}
	for _, f := range files {
		if filepath.Ext(f.Name()) == ".json" {
			data, err := os.ReadFile(filepath.Join(m.dir, f.Name()))
			if err != nil {
				continue
			}
			var s DashboardSession
			if err := json.Unmarshal(data, &s); err == nil {
				m.sessions[s.DashboardID] = &s
			}
		}
	}
}

func (m *SessionManager) StartCleanupLoop(ctx context.Context, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			m.cleanup(now, ttl)
		}
	}
}

func (m *SessionManager) cleanup(now time.Time, ttl time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, s := range m.sessions {
		if now.Sub(s.TouchedAt) > ttl {
			delete(m.sessions, id)
			_ = os.Remove(filepath.Join(m.dir, id+".json"))
			if m.OnEvict != nil {
				// Called within lock, should be fast or run in goroutine
				// but since it's just logging it's fine.
				go m.OnEvict(id)
			}
		}
	}
}
