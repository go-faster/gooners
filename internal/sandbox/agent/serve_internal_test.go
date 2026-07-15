package agent

import (
	"crypto/ed25519"
	"crypto/rand"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

func testSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := ssh.NewSignerFromKey(priv)
	require.NoError(t, err)
	return signer
}

func TestConfig_setDefaults_MissingKeys(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"missing both", Config{}},
		{"missing authorized key", Config{HostKey: testSigner(t)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.setDefaults()
			require.Error(t, err)
		})
	}
}

func TestConfig_setDefaults_Applies(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell (/bin/bash or /bin/sh)")
	}

	signer := testSigner(t)
	cfg := Config{HostKey: signer, AuthorizedKey: signer.PublicKey()}
	require.NoError(t, cfg.setDefaults())

	require.NotEmpty(t, cfg.Shell)
	require.Equal(t, "dev", cfg.Version)
	require.NotNil(t, cfg.Logger)
}

func TestConfig_setDefaults_ExplicitShellPreserved(t *testing.T) {
	// No OS skip needed: an explicit Shell is never probed with LookPath.
	signer := testSigner(t)
	cfg := Config{HostKey: signer, AuthorizedKey: signer.PublicKey(), Shell: "/bin/sh", Version: "1.2.3"}
	require.NoError(t, cfg.setDefaults())

	require.Equal(t, "/bin/sh", cfg.Shell)
	require.Equal(t, "1.2.3", cfg.Version)
}
