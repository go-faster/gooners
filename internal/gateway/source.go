package gateway

import (
	"context"
	"crypto/sha256"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/go-faster/errors"
)

// Source supplies gateway configuration to a [Reloader].
type Source interface {
	// Load returns the configuration as it is right now.
	Load(ctx context.Context) (*Config, error)
	// Watch blocks until ctx is done, sending on ch every time the underlying
	// configuration may have changed. It never sends the configuration itself:
	// the reader calls Load, so a source that changes twice in quick succession
	// costs one reload rather than two.
	Watch(ctx context.Context, ch chan<- struct{}) error
}

// FileSource loads configuration from a TOML file, and reports changes on
// SIGHUP and by polling the file's content hash.
//
// Hashing the content, rather than watching inotify events or comparing mtimes,
// is what makes this work for the ways config files are actually replaced:
// atomic rename, ConfigMap symlink swap, and editor save-with-backup all break
// an inotify watch on the original inode, while a rewrite with identical bytes
// (a redeploy of an unchanged file) must not churn upstream connections.
type FileSource struct {
	path     string
	interval time.Duration
	signals  []os.Signal

	mu   sync.Mutex
	last [sha256.Size]byte
}

// FileSourceOptions configures [NewFileSource].
type FileSourceOptions struct {
	// Path to the TOML config file. Required.
	Path string
	// Interval between content-hash polls. Zero disables polling, leaving
	// Signals as the only trigger.
	Interval time.Duration
	// Signals that trigger a reload. Nil means SIGHUP; an empty non-nil slice
	// disables signal handling.
	Signals []os.Signal
}

func (o *FileSourceOptions) setDefaults() {
	if o.Signals == nil {
		o.Signals = []os.Signal{syscall.SIGHUP}
	}
}

// NewFileSource returns a [Source] reading opts.Path.
func NewFileSource(opts FileSourceOptions) (*FileSource, error) {
	opts.setDefaults()
	if opts.Path == "" {
		return nil, errors.New("path is required")
	}
	return &FileSource{
		path:     opts.Path,
		interval: opts.Interval,
		signals:  opts.Signals,
	}, nil
}

// Path returns the watched file path.
func (s *FileSource) Path() string { return s.path }

// Load reads and decodes the file, and records its hash so a poll that follows
// an explicit load does not report a spurious change.
func (s *FileSource) Load(context.Context) (*Config, error) {
	data, err := os.ReadFile(s.path) //nolint:gosec // G304: operator-controlled config file path
	if err != nil {
		return nil, errors.Wrap(err, "read config")
	}
	cfg, err := Decode(data)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.last = sha256.Sum256(data)
	s.mu.Unlock()
	return cfg, nil
}

// Watch implements [Source].
func (s *FileSource) Watch(ctx context.Context, ch chan<- struct{}) error {
	sigCh := make(chan os.Signal, 1)
	if len(s.signals) > 0 {
		signal.Notify(sigCh, s.signals...)
		defer signal.Stop(sigCh)
	}

	var tick <-chan time.Time
	if s.interval > 0 {
		t := time.NewTicker(s.interval)
		defer t.Stop()
		tick = t.C
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-sigCh:
			// A signal is an explicit operator request: reload even if the
			// bytes are unchanged, since secrets it interpolates may not be.
		case <-tick:
			if !s.changed() {
				continue
			}
		}
		select {
		case ch <- struct{}{}:
		case <-ctx.Done():
			return nil
		}
	}
}

// changed reports whether the file content differs from the last one seen. An
// unreadable file is not a change: the config is left alone until it comes back,
// and reappearing with different content still reports true.
func (s *FileSource) changed() bool {
	data, err := os.ReadFile(s.path) //nolint:gosec // G304: operator-controlled config file path
	if err != nil {
		return false
	}
	sum := sha256.Sum256(data)
	s.mu.Lock()
	defer s.mu.Unlock()
	if sum == s.last {
		return false
	}
	s.last = sum
	return true
}
