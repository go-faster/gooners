package mcpcmd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestTransportFlags_Run_RejectsExposeFlagsWithStdio(t *testing.T) {
	flags := TransportFlags{Transport: "stdio", ExposeName: "foo"}
	err := flags.Run(context.Background(), RunOptions{Name: "srv", Server: mcp.NewServer(&mcp.Implementation{Name: "srv", Version: "0"}, nil)})
	require.Error(t, err)
	require.Contains(t, err.Error(), "stdio")
}

func TestTransportFlags_Run_RejectsTLSFlagsWithStdio(t *testing.T) {
	flags := TransportFlags{Transport: "stdio", TLSCertFile: "cert.pem", TLSKeyFile: "key.pem"}
	err := flags.Run(context.Background(), RunOptions{Name: "srv", Server: mcp.NewServer(&mcp.Implementation{Name: "srv", Version: "0"}, nil)})
	require.Error(t, err)
	require.Contains(t, err.Error(), "TLS")
}

func TestTransportFlags_TLSConfig_RequiresCertAndKey(t *testing.T) {
	_, err := TransportFlags{TLSCertFile: "cert.pem"}.tlsConfig()
	require.ErrorContains(t, err, "must be set together")
}

func TestTransportFlags_ResolveExposeProvider(t *testing.T) {
	tests := []struct {
		name    string
		flags   TransportFlags
		want    string
		wantErr bool
	}{
		{name: "default cloudflare by name", flags: TransportFlags{ExposeName: "foo"}, want: "cloudflare"},
		{name: "default cloudflare by config", flags: TransportFlags{ExposeConfig: "cfg"}, want: "cloudflare"},
		{name: "ngrok ok", flags: TransportFlags{ExposeProvider: "ngrok"}, want: "ngrok"},
		{name: "ngrok with cloudflare-only flags", flags: TransportFlags{ExposeProvider: "ngrok", ExposeName: "foo"}, wantErr: true},
		{name: "cloudflare tcp invalid", flags: TransportFlags{ExposeProvider: "cloudflare", ExposeType: "tcp"}, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.flags.resolveExposeProvider()
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestHealthHandler(t *testing.T) {
	h := healthHandler("srv")

	req := httptest.NewRequest(http.MethodGet, "/health", http.NoBody)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"status":"ok","server":"srv"}`, rec.Body.String())
}

// TestReadyHandler: /readyz answers a different question than /health, and a
// server that is up but not usable must say so with a 503 rather than a 200.
func TestReadyHandler(t *testing.T) {
	for _, tt := range []struct {
		name  string
		ready func() error
		want  int
		body  string
	}{
		{
			name: "NilIsReady",
			want: http.StatusOK,
			body: `{"status":"ready","server":"srv"}`,
		},
		{
			name:  "Ready",
			ready: func() error { return nil },
			want:  http.StatusOK,
			body:  `{"status":"ready","server":"srv"}`,
		},
		{
			name:  "NotReady",
			ready: func() error { return errors.New("initial build has not finished") },
			want:  http.StatusServiceUnavailable,
			body:  `{"status":"not_ready","server":"srv","reason":"initial build has not finished"}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			readyHandler("srv", tt.ready).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", http.NoBody))

			require.Equal(t, tt.want, rec.Code)
			require.JSONEq(t, tt.body, rec.Body.String())
		})
	}
}

func TestTransportFlags_ApplyExposeDefaults(t *testing.T) {
	flags := TransportFlags{ExposeName: "foo"}

	require.NoError(t, flags.applyExposeDefaults())
	require.Equal(t, "cloudflare", flags.ExposeProvider)
	require.True(t, flags.DisableLocalhostProtection)
}
