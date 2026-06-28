package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseLocalForward(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		raw        string
		want       localForward
		wantErrMsg string
	}{
		{
			name: "port only listen",
			raw:  "8080 127.0.0.1:80",
			want: localForward{
				listenAddr: "127.0.0.1:8080",
				targetAddr: "127.0.0.1:80",
			},
		},
		{
			name: "explicit listen host",
			raw:  "0.0.0.0:8080 localhost:80",
			want: localForward{
				listenAddr: "0.0.0.0:8080",
				targetAddr: "localhost:80",
			},
		},
		{
			name: "bracketed ipv6 target",
			raw:  "127.0.0.1:8080 [::1]:80",
			want: localForward{
				listenAddr: "127.0.0.1:8080",
				targetAddr: "[::1]:80",
			},
		},
		{
			name:       "invalid field count",
			raw:        "8080",
			wantErrMsg: "unsupported LocalForward \"8080\"",
		},
		{
			name:       "invalid target address",
			raw:        "8080 localhost:80:abc",
			wantErrMsg: "local forward target address: invalid port \"abc\"",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			t.Logf("input: %q", tc.raw)

			got, err := parseLocalForward(tc.raw)
			if tc.wantErrMsg != "" {
				require.Error(t, err)
				require.ErrorContains(t, err, tc.wantErrMsg)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestParseForwardListenAddr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		spec       string
		want       string
		wantErrMsg string
	}{
		{name: "port only", spec: "8080", want: "127.0.0.1:8080"},
		{name: "empty host", spec: ":8080", wantErrMsg: "missing host in \":8080\""},
		{name: "explicit host", spec: "0.0.0.0:8080", want: "0.0.0.0:8080"},
		{name: "ipv6 host", spec: "[::1]:8080", want: "[::1]:8080"},
		{name: "fallback invalid port", spec: "localhost:80:abc", wantErrMsg: "invalid port \"abc\""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			t.Logf("input: %q", tc.spec)

			got, err := parseForwardListenAddr(tc.spec)
			if tc.wantErrMsg != "" {
				require.Error(t, err)
				require.ErrorContains(t, err, tc.wantErrMsg)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestParseForwardTargetAddr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		spec       string
		want       string
		wantErrMsg string
	}{
		{name: "hostname", spec: "localhost:80", want: "localhost:80"},
		{name: "ipv6 host", spec: "[::1]:80", want: "[::1]:80"},
		{name: "fallback invalid port", spec: "localhost:80:abc", wantErrMsg: "invalid port \"abc\""},
		{name: "fallback missing port", spec: "localhost:80:", wantErrMsg: "missing port in \"localhost:80:\""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			t.Logf("input: %q", tc.spec)

			got, err := parseForwardTargetAddr(tc.spec)
			if tc.wantErrMsg != "" {
				require.Error(t, err)
				require.ErrorContains(t, err, tc.wantErrMsg)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestSplitForwardAddr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		spec       string
		wantHost   string
		wantPort   string
		wantErrMsg string
	}{
		{name: "hostname", spec: "localhost:80", wantHost: "localhost", wantPort: "80"},
		{name: "ipv6 host", spec: "[::1]:80", wantHost: "::1", wantPort: "80"},
		{name: "fallback invalid port", spec: "localhost:80:abc", wantErrMsg: "invalid port \"abc\""},
		{name: "fallback missing port", spec: "localhost:80:", wantErrMsg: "missing port in \"localhost:80:\""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			t.Logf("input: %q", tc.spec)

			gotHost, gotPort, err := splitForwardAddr(tc.spec)
			if tc.wantErrMsg != "" {
				require.Error(t, err)
				require.ErrorContains(t, err, tc.wantErrMsg)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantHost, gotHost)
			require.Equal(t, tc.wantPort, gotPort)
		})
	}
}
