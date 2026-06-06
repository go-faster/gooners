package session

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	os.Unsetenv("SSH_AUTH_SOCK")
	os.Exit(m.Run())
}
