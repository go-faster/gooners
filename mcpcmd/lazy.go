package mcpcmd

import (
	"context"
	"io"
	"sync"

	"github.com/go-faster/gooners/blob"
)

// lazyStore defers building a store until something actually needs it, and
// keeps trying until it succeeds.
//
// A store that authenticates against a remote while being constructed turns
// that remote into a startup dependency of the whole process. For a gateway
// fronting a dozen upstreams that is badly out of proportion: object storage
// being down would take out every tool, none of which use it. Deferring the
// build inverts that — the process starts, only the blob tools fail, and they
// say why. Recovery needs no restart either, because the next call builds the
// store again.
//
// The build is attempted once eagerly, so a misconfigured bucket is still
// visible in the logs at startup rather than at the first tool call hours
// later.
type lazyStore struct {
	build func(context.Context) (blob.Attacher, error)

	mu    sync.Mutex
	store blob.Attacher
}

// get returns the store, building it if that has not succeeded yet.
func (s *lazyStore) get(ctx context.Context) (blob.Attacher, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.store != nil {
		return s.store, nil
	}
	store, err := s.build(ctx)
	if err != nil {
		return nil, err
	}
	s.store = store
	return store, nil
}

func (s *lazyStore) Put(ctx context.Context, r io.Reader, opts blob.PutOptions) (blob.Blob, error) {
	store, err := s.get(ctx)
	if err != nil {
		return blob.Blob{}, err
	}
	return store.Put(ctx, r, opts)
}

func (s *lazyStore) Open(ctx context.Context, id string) (io.ReadSeekCloser, blob.Blob, error) {
	store, err := s.get(ctx)
	if err != nil {
		return nil, blob.Blob{}, err
	}
	return store.Open(ctx, id)
}

func (s *lazyStore) Delete(ctx context.Context, id string) error {
	store, err := s.get(ctx)
	if err != nil {
		return err
	}
	return store.Delete(ctx, id)
}

func (s *lazyStore) Attach(ctx context.Context, src blob.FS, name string, opts blob.PutOptions) (blob.Blob, error) {
	store, err := s.get(ctx)
	if err != nil {
		return blob.Blob{}, err
	}
	return store.Attach(ctx, src, name, opts)
}
