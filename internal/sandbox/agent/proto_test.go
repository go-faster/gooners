package agent

import (
	"bufio"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWriteReadPreamble_RoundTrip(t *testing.T) {
	cases := []struct {
		name string
		p    Preamble
	}{
		{"minimal", Preamble{Version: 1, HostKeyPEM: "host", AuthorizedKey: "auth"}},
		{"full", Preamble{Version: 1, HostKeyPEM: "host-pem", AuthorizedKey: "ssh-ed25519 AAAA...", Shell: "/bin/bash", Workdir: "/work"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf strings.Builder
			require.NoError(t, WritePreamble(&buf, tc.p))

			r := bufio.NewReader(strings.NewReader(buf.String()))
			got, err := ReadPreamble(r)
			require.NoError(t, err)
			require.Equal(t, tc.p, got)
		})
	}
}

func TestWritePreamble_LineFormat(t *testing.T) {
	var buf strings.Builder
	require.NoError(t, WritePreamble(&buf, Preamble{Version: 1}))

	require.True(t, strings.HasPrefix(buf.String(), Magic+" "))
	require.True(t, strings.HasSuffix(buf.String(), "\n"))
}

func TestReadPreamble_LeavesTrailingBytesBuffered(t *testing.T) {
	var buf strings.Builder
	require.NoError(t, WritePreamble(&buf, Preamble{Version: 1, HostKeyPEM: "h", AuthorizedKey: "a"}))
	buf.WriteString("trailing-handshake-bytes")

	r := bufio.NewReader(strings.NewReader(buf.String()))
	_, err := ReadPreamble(r)
	require.NoError(t, err)

	rest := make([]byte, len("trailing-handshake-bytes"))
	n, err := r.Read(rest)
	require.NoError(t, err)
	require.Equal(t, "trailing-handshake-bytes", string(rest[:n]))
}

func TestReadPreamble_Errors(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"missing magic", "NOTMAGIC eyJ2IjoxfQ==\n"},
		{"bad base64", Magic + " not-base64!!!\n"},
		{"bad json", Magic + " " + "bm90LWpzb24=" + "\n"}, // decodes to bytes that aren't valid JSON
		{"empty input", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := bufio.NewReader(strings.NewReader(tc.line))
			_, err := ReadPreamble(r)
			require.Error(t, err)
		})
	}
}
