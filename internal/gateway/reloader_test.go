package gateway

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/go-faster/errors"
	"github.com/stretchr/testify/require"
)

// fakeSource is a [Source] driven by the test rather than by a file.
type fakeSource struct {
	trigger chan struct{}

	mu      sync.Mutex
	cfg     *Config
	loadErr error
}

func newFakeSource(cfg *Config) *fakeSource {
	return &fakeSource{trigger: make(chan struct{}, 1), cfg: cfg}
}

func (s *fakeSource) Load(context.Context) (*Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	return s.cfg, nil
}

func (s *fakeSource) set(cfg *Config, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg, s.loadErr = cfg, err
}

func (s *fakeSource) Watch(ctx context.Context, ch chan<- struct{}) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.trigger:
			select {
			case ch <- struct{}{}:
			case <-ctx.Done():
				return nil
			}
		}
	}
}

// fakeTarget records the configs it was asked to apply.
type fakeTarget struct {
	mu      sync.Mutex
	applied []*Config
	err     error
	res     ReloadResult
}

func (t *fakeTarget) Reload(_ context.Context, cfg *Config) (ReloadResult, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.applied = append(t.applied, cfg)
	return t.res, t.err
}

func (t *fakeTarget) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.applied)
}

func testConfig() *Config {
	return &Config{
		Server:    ServerConfig{Name: "gw"},
		Upstreams: []UpstreamConfig{upstreamCfg("u1")},
	}
}

func TestNewReloader_Requires(t *testing.T) {
	_, err := NewReloader(ReloaderOptions{Target: &fakeTarget{}})
	require.ErrorContains(t, err, "source is required")
	_, err = NewReloader(ReloaderOptions{Source: newFakeSource(testConfig())})
	require.ErrorContains(t, err, "target is required")
}

func TestReloader_Reload(t *testing.T) {
	cfg := testConfig()
	src := newFakeSource(cfg)
	target := &fakeTarget{res: ReloadResult{Added: []string{"u1"}}}

	var got []ReloadResult
	r, err := NewReloader(ReloaderOptions{
		Source:   src,
		Target:   target,
		OnReload: func(res ReloadResult, _ error) { got = append(got, res) },
	})
	require.NoError(t, err)

	res, err := r.Reload(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{"u1"}, res.Added)
	require.Equal(t, []*Config{cfg}, target.applied)
	require.Len(t, got, 1)
}

// A source that cannot produce a config must not reach the target at all.
func TestReloader_LoadFailureDoesNotReachTarget(t *testing.T) {
	src := newFakeSource(nil)
	src.set(nil, errors.New("boom"))
	target := &fakeTarget{}

	var gotErr error
	r, err := NewReloader(ReloaderOptions{
		Source:   src,
		Target:   target,
		OnReload: func(_ ReloadResult, err error) { gotErr = err },
	})
	require.NoError(t, err)

	_, err = r.Reload(t.Context())
	require.ErrorContains(t, err, "boom")
	require.Error(t, gotErr)
	require.Zero(t, target.count())
}

// A target that rejects a config must not stop the reloader from trying again.
func TestReloader_RunSurvivesFailures(t *testing.T) {
	src := newFakeSource(testConfig())
	target := &fakeTarget{err: errors.New("rejected")}

	reloaded := make(chan error, 4)
	r, err := NewReloader(ReloaderOptions{
		Source:   src,
		Target:   target,
		OnReload: func(_ ReloadResult, err error) { reloaded <- err },
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	src.trigger <- struct{}{}
	requireReload(t, reloaded, true)

	target.mu.Lock()
	target.err = nil
	target.mu.Unlock()
	src.trigger <- struct{}{}
	requireReload(t, reloaded, false)

	require.Equal(t, 2, target.count())
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop")
	}
}

func requireReload(t *testing.T, ch <-chan error, wantErr bool) {
	t.Helper()
	select {
	case err := <-ch:
		if wantErr {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no reload observed")
	}
}

// End to end: a file change reaches a real gateway through a real FileSource.
func TestReloader_FileSourceToGateway(t *testing.T) {
	const initial = `
[server]
name = "gw"

[[upstream]]
name = "u1"
kind = "stdio"
command = ["ignored"]
`
	const updated = initial + `
[[upstream]]
name = "u2"
kind = "stdio"
command = ["ignored"]
`
	path := writeConfig(t, initial)
	src, err := NewFileSource(FileSourceOptions{Path: path, Interval: time.Millisecond})
	require.NoError(t, err)
	cfg, err := src.Load(t.Context())
	require.NoError(t, err)

	g := newTestGateway(t, cfg, map[string][]string{"u1": {"a"}, "u2": {"b"}})
	require.Equal(t, []string{"a"}, registeredNames(g))

	reloaded := make(chan error, 1)
	r, err := NewReloader(ReloaderOptions{
		Source:   src,
		Target:   g,
		OnReload: func(_ ReloadResult, err error) { reloaded <- err },
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = r.Run(ctx) }()

	require.NoError(t, os.WriteFile(path, []byte(updated), 0o600))
	requireReload(t, reloaded, false)
	require.Equal(t, []string{"a", "b"}, registeredNames(g))
}
