package cmdutil

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestTransportFlags_Run_RejectsExposeFlagsWithStdio(t *testing.T) {
	flags := TransportFlags{Transport: "stdio", ExposeName: "foo"}
	err := flags.Run(context.Background(), "srv", mcp.NewServer(&mcp.Implementation{Name: "srv", Version: "0"}, nil), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "stdio")
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
