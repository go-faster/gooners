package effect

import (
	"io/fs"
	"os"
	"path/filepath"
)

// OS returns an unconfined FS backed by the [os] package.
//
// It enforces nothing, so it belongs only where the paths are the operator's
// (startup config, CLI flags) or the test's — never where a path can reach an
// agent. Confine agent-reachable paths with [Root].
func OS() FS { return osFS{} }

type osFS struct{}

func (osFS) Open(name string) (File, error) {
	f, err := os.Open(name) //nolint:gosec // unconfined by construction; see [OS]
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (osFS) Create(name string) (File, error) {
	f, err := os.Create(name) //nolint:gosec // unconfined by construction; see [OS]
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (osFS) ReadFile(name string) ([]byte, error) {
	return os.ReadFile(name) //nolint:gosec // unconfined by construction; see [OS]
}

func (osFS) WriteFile(name string, data []byte, perm fs.FileMode) error {
	return os.WriteFile(name, data, perm) //nolint:gosec // unconfined by construction; see [OS]
}

func (osFS) Resolve(name string) (string, error)          { return filepath.Abs(name) }
func (osFS) Stat(name string) (fs.FileInfo, error)        { return os.Stat(name) }
func (osFS) MkdirAll(name string, perm fs.FileMode) error { return os.MkdirAll(name, perm) }
func (osFS) Remove(name string) error                     { return os.Remove(name) }
func (osFS) RemoveAll(name string) error                  { return os.RemoveAll(name) }
func (osFS) Rename(oldpath, newpath string) error         { return os.Rename(oldpath, newpath) }
