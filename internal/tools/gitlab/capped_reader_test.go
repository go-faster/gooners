package gitlab

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// eofWithData returns its last bytes together with io.EOF, which is what a
// net/http body does and what makes an overrun invisible to a consumer that
// stops at the first EOF.
type eofWithData struct {
	data []byte
}

func (r *eofWithData) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, io.EOF
	}
	return n, nil
}

func TestCappedReader(t *testing.T) {
	const limit = 4

	tests := []struct {
		name    string
		reader  func(size int) io.Reader
		size    int
		wantErr bool
	}{
		{"under the limit", func(n int) io.Reader { return strings.NewReader(strings.Repeat("x", n)) }, limit - 1, false},
		{"at the limit", func(n int) io.Reader { return strings.NewReader(strings.Repeat("x", n)) }, limit, false},
		{"over the limit", func(n int) io.Reader { return strings.NewReader(strings.Repeat("x", n)) }, limit + 1, true},
		{"at the limit, eof with data", func(n int) io.Reader { return &eofWithData{data: []byte(strings.Repeat("x", n))} }, limit, false},
		{"over the limit, eof with data", func(n int) io.Reader { return &eofWithData{data: []byte(strings.Repeat("x", n))} }, limit + 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &cappedReader{r: tt.reader(tt.size), n: limit + 1, name: "asset"}

			n, err := io.Copy(io.Discard, c)
			if tt.wantErr {
				require.ErrorContains(t, err, "over the")
				return
			}
			require.NoError(t, err)
			require.Equal(t, int64(tt.size), n)
		})
	}
}
