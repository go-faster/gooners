package agent

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

// FuzzReadPreamble seeds from every case in [TestReadPreamble_Errors] plus
// the encoded output of [WritePreamble] for every valid case in
// [TestWriteReadPreamble_RoundTrip], then checks that ReadPreamble never
// panics and, when it succeeds on a WritePreamble-encoded input, recovers
// the original Preamble.
func FuzzReadPreamble(f *testing.F) {
	// Failure-case seeds (raw bytes, not necessarily valid preambles).
	errCases := []string{
		"NOTMAGIC eyJ2IjoxfQ==\n",
		Magic + " not-base64!!!\n",
		Magic + " " + "bm90LWpzb24=" + "\n",
		"",
	}
	for _, s := range errCases {
		f.Add([]byte(s))
	}

	// Valid-case seeds: encode via WritePreamble itself so seeds track the
	// real wire format rather than a hand-rolled guess at it.
	validCases := []Preamble{
		{Version: 1, HostKeyPEM: "host", AuthorizedKey: "auth"},
		{Version: 1, HostKeyPEM: "host-pem", AuthorizedKey: "ssh-ed25519 AAAA...", Shell: "/bin/bash", Workdir: "/work"},
		{Version: 1, HostKeyPEM: "h", AuthorizedKey: "a"},
	}
	for _, p := range validCases {
		var buf strings.Builder
		if err := WritePreamble(&buf, p); err != nil {
			f.Fatalf("seeding: WritePreamble: %v", err)
		}
		f.Add([]byte(buf.String()))
	}
	// Same as above but with trailing bytes after the preamble line, like a
	// real handshake stream would have.
	{
		var buf strings.Builder
		if err := WritePreamble(&buf, validCases[0]); err != nil {
			f.Fatalf("seeding: WritePreamble: %v", err)
		}
		buf.WriteString("trailing-handshake-bytes")
		f.Add([]byte(buf.String()))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		r := bufio.NewReader(bytes.NewReader(data))
		got, err := ReadPreamble(r)
		if err != nil {
			return
		}

		// Round-trip check: if data is exactly a WritePreamble encoding of
		// got, re-encoding and re-decoding must reproduce it.
		var buf bytes.Buffer
		if err := WritePreamble(&buf, got); err != nil {
			t.Fatalf("WritePreamble on decoded Preamble failed: %v", err)
		}
		r2 := bufio.NewReader(&buf)
		got2, err := ReadPreamble(r2)
		if err != nil {
			t.Fatalf("re-decoding a freshly re-encoded Preamble failed: %v", err)
		}
		if got != got2 {
			t.Fatalf("round-trip mismatch: got=%+v got2=%+v", got, got2)
		}
	})
}
