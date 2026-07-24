package gitlab

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseAuthMode(t *testing.T) {
	for _, tt := range []struct {
		in      string
		want    AuthMode
		wantErr bool
	}{
		{in: "", want: AuthServer},
		{in: "server", want: AuthServer},
		{in: "client", want: AuthClientRequired},
		{in: "client-optional", want: AuthClientOptional},
		{in: "Server", wantErr: true},
		{in: "none", wantErr: true},
	} {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseAuthMode(tt.in)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
			require.NotEqual(t, "unknown", got.String())
		})
	}
}

func TestClientToken(t *testing.T) {
	for _, tt := range []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{name: "none", want: ""},
		{name: "private token", headers: map[string]string{"PRIVATE-TOKEN": "glpat-1"}, want: "glpat-1"},
		{name: "bearer", headers: map[string]string{"Authorization": "Bearer glpat-2"}, want: "glpat-2"},
		{name: "trims", headers: map[string]string{"Authorization": "Bearer  glpat-3 "}, want: "glpat-3"},
		{
			name:    "private token wins",
			headers: map[string]string{"PRIVATE-TOKEN": "glpat-1", "Authorization": "Bearer glpat-2"},
			want:    "glpat-1",
		},
		{
			name:    "falls through a blank private token",
			headers: map[string]string{"PRIVATE-TOKEN": "  ", "Authorization": "Bearer glpat-2"},
			want:    "glpat-2",
		},
		{name: "basic auth is not a token", headers: map[string]string{"Authorization": "Basic abc"}, want: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", http.NoBody)
			for k, v := range tt.headers {
				r.Header.Set(k, v)
			}
			require.Equal(t, tt.want, ClientToken(r))
		})
	}

	t.Run("a nil request is stdio", func(t *testing.T) {
		require.Empty(t, ClientToken(nil))
	})
}
