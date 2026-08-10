package mcpcmd

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/go-faster/errors"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/blob"
)

// fakeStore records that it was reached.
type fakeStore struct{ puts int }

func (f *fakeStore) Put(context.Context, io.Reader, blob.PutOptions) (blob.Blob, error) {
	f.puts++
	return blob.Blob{ID: "ns/id"}, nil
}

func (f *fakeStore) Open(context.Context, string) (io.ReadSeekCloser, blob.Blob, error) {
	return nil, blob.Blob{}, nil
}
func (f *fakeStore) Delete(context.Context, string) error { return nil }
func (f *fakeStore) Attach(context.Context, blob.FS, string, blob.PutOptions) (blob.Blob, error) {
	return blob.Blob{ID: "ns/attached"}, nil
}

// TestLazyStoreRetriesUntilItCanBuild is the whole point of the type: a remote
// that is down when the process starts must not be down forever from the
// process's point of view.
func TestLazyStoreRetriesUntilItCanBuild(t *testing.T) {
	var attempts int
	fake := &fakeStore{}
	s := &lazyStore{build: func(context.Context) (blob.Attacher, error) {
		attempts++
		if attempts < 3 {
			return nil, errors.New("connection refused")
		}
		return fake, nil
	}}

	// The eager probe fails, and so does a call made while it is still down.
	_, err := s.get(t.Context())
	require.ErrorContains(t, err, "connection refused")

	_, err = s.Put(t.Context(), strings.NewReader("x"), blob.PutOptions{})
	require.ErrorContains(t, err, "connection refused", "the failure reaches the caller")
	require.Equal(t, 0, fake.puts)

	// Third time the endpoint is back: no restart involved.
	b, err := s.Put(t.Context(), strings.NewReader("x"), blob.PutOptions{})
	require.NoError(t, err)
	require.Equal(t, "ns/id", b.ID)
	require.Equal(t, 1, fake.puts)
	require.Equal(t, 3, attempts)

	// Once built it is cached, not rebuilt per call.
	_, err = s.Attach(t.Context(), nil, "f.txt", blob.PutOptions{})
	require.NoError(t, err)
	require.Equal(t, 3, attempts)
}

// TestSetupS3DoesNotFailOnUnreachableEndpoint: the process must still come up.
// This is the behavior that keeps object storage from becoming a startup
// dependency of every unrelated tool a binary serves.
func TestSetupS3DoesNotFailOnUnreachableEndpoint(t *testing.T) {
	flags := BlobFlags{
		// Loopback on a port nothing listens on: the dial is refused at once,
		// where a blackholed address would hang until the TCP timeout and make
		// this test take a minute and a half.
		S3Endpoint: "http://127.0.0.1:1",
		S3Bucket:   "nope",
	}
	store, run, err := flags.Setup(t.Context(), BlobOptions{Name: "test", Logger: testLogger()})
	require.NoError(t, err, "an unreachable endpoint is not a startup failure")
	require.NotNil(t, store)
	require.NoError(t, run(t.Context()), "the S3 backend runs nothing of its own")

	// The tool that uses it still fails, and says so, rather than silently
	// pretending to have stored anything.
	_, err = store.Put(t.Context(), strings.NewReader("x"), blob.PutOptions{Name: "f.txt"})
	require.Error(t, err)
}
