package blob

import (
	"io/fs"

	"github.com/go-faster/gooners/internal/effect"
)

// Dir returns an [FS] confined to dir: a path resolving outside it is refused,
// symlinks included. dir is created (0700) on first use.
//
// It is the repository's own filesystem provider rather than a second
// implementation, so a store gets exactly the confinement the tools get. The
// provider is adapted rather than returned directly because Go interface types
// are identical only when they are the same declaration, never merely when
// they have the same methods.
func Dir(dir string) FS { return effectFS{effect.Root(dir)} }

// effectFS adapts the repository's filesystem provider to [FS]. Every method
// is a pass-through; only the [File] interface changes, and any value
// satisfying one satisfies the other.
type effectFS struct{ fs effect.FS }

func (e effectFS) Open(name string) (File, error) { return e.fs.Open(name) }

func (e effectFS) Create(name string) (File, error) { return e.fs.Create(name) }

func (e effectFS) Stat(name string) (fs.FileInfo, error) { return e.fs.Stat(name) }

func (e effectFS) MkdirAll(name string, perm fs.FileMode) error { return e.fs.MkdirAll(name, perm) }

func (e effectFS) Remove(name string) error { return e.fs.Remove(name) }

func (e effectFS) RemoveAll(name string) error { return e.fs.RemoveAll(name) }
