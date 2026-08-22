package gitlab

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/go-faster/errors"
	"github.com/stretchr/testify/require"
)

func TestCappedReader(t *testing.T) {
	const limit = 8

	// A capped reader has to hold whatever shape the reader underneath it
	// hands back: one byte at a time, short reads, and — the case that made an
	// oversized asset invisible — the final bytes arriving together with
	// io.EOF, which is what a net/http body does.
	wrappers := []struct {
		name string
		wrap func(io.Reader) io.Reader
	}{
		{"plain", func(r io.Reader) io.Reader { return r }},
		{"one byte", iotest.OneByteReader},
		{"half", iotest.HalfReader},
		{"data err", iotest.DataErrReader},
		{"one byte data err", func(r io.Reader) io.Reader { return iotest.OneByteReader(iotest.DataErrReader(r)) }},
	}
	sizes := []struct {
		name    string
		size    int
		wantErr bool
	}{
		{"under the limit", limit - 1, false},
		{"at the limit", limit, false},
		{"one byte over", limit + 1, true},
		{"far over", limit * 4, true},
	}

	for _, w := range wrappers {
		for _, s := range sizes {
			t.Run(w.name+", "+s.name, func(t *testing.T) {
				data := strings.Repeat("x", s.size)
				c := &cappedReader{r: w.wrap(strings.NewReader(data)), n: limit + 1, name: "asset"}

				var out bytes.Buffer
				_, err := io.Copy(&out, c)
				if s.wantErr {
					require.ErrorContains(t, err, "over the")
					require.LessOrEqual(t, out.Len(), limit+1, "no more than the allowance is handed on")
					return
				}
				require.NoError(t, err)
				require.Equal(t, data, out.String())
			})
		}
	}
}

// An error from the asset transfer is the caller's to see, not something the
// cap turns into its own message.
func TestCappedReaderPropagatesError(t *testing.T) {
	errBroken := errors.New("broken")
	c := &cappedReader{r: iotest.ErrReader(errBroken), n: 8, name: "asset"}

	_, err := io.Copy(io.Discard, c)
	require.ErrorIs(t, err, errBroken)
}

// The under-limit path must behave like any other reader.
func TestCappedReaderIsWellBehaved(t *testing.T) {
	const limit = 8

	require.NoError(t, iotest.TestReader(
		&cappedReader{r: strings.NewReader(strings.Repeat("x", limit)), n: limit + 1, name: "asset"},
		[]byte(strings.Repeat("x", limit)),
	))
}
