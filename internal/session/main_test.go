package session

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	tmpDir, err := os.MkdirTemp("", "gooners-test-home-*")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", tmpDir)
	os.Unsetenv("SSH_AUTH_SOCK")
	code := m.Run()
	os.RemoveAll(tmpDir)
	os.Exit(code)
}
