package effect

import (
	"io"
	"io/fs"
)

// FS is the host filesystem provider. It mirrors the [os] functions the tools
// need, so a call site reads the same as the code it replaces, but every path
// is subject to the provider's policy.
//
// Paths may be absolute or relative to the provider's root; a confined
// provider rejects any path that resolves outside it, symlinks included.
type FS interface {
	Open(name string) (File, error)
	Create(name string) (File, error)
	ReadFile(name string) ([]byte, error)
	WriteFile(name string, data []byte, perm fs.FileMode) error
	Stat(name string) (fs.FileInfo, error)
	MkdirAll(name string, perm fs.FileMode) error
	Remove(name string) error
	RemoveAll(name string) error
	Rename(oldpath, newpath string) error

	// Resolve reports the host path name maps to, or an error if the provider
	// would refuse it. It exists so a caller can reject a bad path up front,
	// with a message naming it, rather than failing halfway through an async
	// job.
	//
	// It is NOT the gate, and a caller is never required to call it: the
	// operations above enforce the same policy themselves, and they enforce
	// more of it (Resolve cannot see a symlink that has yet to be traversed).
	// Treat it as a diagnostic, not a permission check.
	Resolve(name string) (string, error)
}

// File is an open file handle from an [FS].
type File interface {
	io.ReadWriteSeeker
	io.Closer
	Stat() (fs.FileInfo, error)
}

// Deny returns an FS that refuses every operation with [ErrDenied], wrapped
// with reason. It is the right zero value for a process that must not touch
// host files at all (sandbox-mcp), and it keeps call sites free of nil checks.
func Deny(reason string) FS { return denyFS{reason: reason} }
