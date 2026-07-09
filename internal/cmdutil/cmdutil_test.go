package cmdutil

import (
	"context"
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

func TestTransportFlags_ApplyExposeDefaults(t *testing.T) {
	flags := TransportFlags{ExposeName: "foo"}

	require.NoError(t, flags.applyExposeDefaults())
	require.Equal(t, "cloudflare", flags.ExposeProvider)
	require.True(t, flags.DisableLocalhostProtection)
}
