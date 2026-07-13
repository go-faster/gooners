package effect

import (
	"io/fs"

	"github.com/go-faster/errors"
)

// denyFS refuses everything. See [Deny].
type denyFS struct{ reason string }

func (d denyFS) err() error { return errors.Wrap(ErrDenied, d.reason) }

func (d denyFS) Resolve(string) (string, error)              { return "", d.err() }
func (d denyFS) Open(string) (File, error)                   { return nil, d.err() }
func (d denyFS) Create(string) (File, error)                 { return nil, d.err() }
func (d denyFS) ReadFile(string) ([]byte, error)             { return nil, d.err() }
func (d denyFS) WriteFile(string, []byte, fs.FileMode) error { return d.err() }
func (d denyFS) Stat(string) (fs.FileInfo, error)            { return nil, d.err() }
func (d denyFS) MkdirAll(string, fs.FileMode) error          { return d.err() }
func (d denyFS) Remove(string) error                         { return d.err() }
func (d denyFS) RemoveAll(string) error                      { return d.err() }
func (d denyFS) Rename(string, string) error                 { return d.err() }
