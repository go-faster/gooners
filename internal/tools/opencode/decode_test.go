package opencode

import (
	"embed"
	"iter"
	"path"
	"testing"

	"github.com/go-faster/jx"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/gooners/internal/tools/opencode/opencodeapi"
)

//go:embed _testdata
var testdata embed.FS

func readRespTestdata(t *testing.T, name string) iter.Seq2[string, []byte] {
	t.Helper()

	dir := path.Join("_testdata", name)
	entries, err := testdata.ReadDir(dir)
	require.NoError(t, err, "read testdata dir %q", dir)

	return func(yield func(string, []byte) bool) {
		for _, e := range entries {
			filename := path.Join(dir, e.Name())
			data, err := testdata.ReadFile(filename)
			require.NoError(t, err, "read testdata file %q", filename)

			if !yield(e.Name(), data) {
				return
			}
		}
	}
}

func testRespTestData[
	P interface {
		*T
		Decode(*jx.Decoder) error
	},
	T any,
](t *testing.T, dirName string) {
	t.Helper()

	for filename, data := range readRespTestdata(t, dirName) {
		t.Run(filename, func(t *testing.T) {
			t.Parallel()

			var val T
			err := P(&val).Decode(jx.DecodeBytes(data))
			require.NoError(t, err)
		})
	}
}

func TestDecode_V2AgentListOK(t *testing.T) {
	testRespTestData[*opencodeapi.V2AgentListOK](t, "agent")
}

func TestDecode_V2ModelListOK(t *testing.T) {
	testRespTestData[*opencodeapi.V2ModelListOK](t, "model")
}

func TestDecode_V2PermissionRequestListOK(t *testing.T) {
	testRespTestData[*opencodeapi.V2PermissionRequestListOK](t, "permission")
}

func TestDecode_SessionsResponse(t *testing.T) {
	testRespTestData[*opencodeapi.SessionsResponse](t, "session")
}
