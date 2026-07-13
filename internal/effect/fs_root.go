package effect

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-faster/errors"
)

// Root returns an FS confined to dir.
//
// Every path is first resolved against dir lexically (an absolute path outside
// dir, or one climbing out with "..", is rejected with [ErrOutsideRoot]), and
// then executed through an [os.Root], which additionally refuses to follow a
// symlink out of dir. The lexical check alone is not enough: it cannot see a
// symlink planted inside the root, which is how a "confined" write ends up on
// an arbitrary host path.
//
// dir is created (0700) and opened on first use rather than here, so a
// provider can be handed to a constructor that has nowhere to report an error.
// A dir that cannot be opened turns every operation into that error.
func Root(dir string) FS { return &rootFS{dir: dir} }

type rootFS struct {
	dir string

	once sync.Once
	root *os.Root
	err  error
}

// open resolves the [os.Root] once, on first use.
func (r *rootFS) open() (*os.Root, error) {
	r.once.Do(func() {
		dir, err := filepath.Abs(r.dir)
		if err != nil {
			r.err = errors.Wrapf(err, "resolve root %q", r.dir)
			return
		}
		r.dir = dir
		if err := os.MkdirAll(dir, 0o700); err != nil {
			r.err = errors.Wrapf(err, "create root %q", dir)
			return
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			r.err = errors.Wrapf(err, "open root %q", dir)
			return
		}
		r.root = root
	})
	return r.root, r.err
}

// rel resolves name to a path relative to the root, rejecting anything that
// lexically escapes it. [os.Root] enforces the same bound again on the way to
// the filesystem, this time including symlinks; this pass exists to fail early
// with an error that names the offending path.
func (r *rootFS) rel(name string) (string, error) {
	if name == "" {
		return "", errors.Wrap(ErrOutsideRoot, "empty path")
	}
	if !filepath.IsAbs(name) {
		name = filepath.Join(r.dir, name)
	}
	rel, err := filepath.Rel(r.dir, filepath.Clean(name))
	if err != nil {
		return "", errors.Wrapf(ErrOutsideRoot, "path %q", name)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", errors.Wrapf(ErrOutsideRoot, "path %q escapes root %q", name, r.dir)
	}
	return rel, nil
}

// resolve is the entry point every operation shares: open the root, then map
// name into it.
func (r *rootFS) resolve(name string) (*os.Root, string, error) {
	root, err := r.open()
	if err != nil {
		return nil, "", err
	}
	rel, err := r.rel(name)
	if err != nil {
		return nil, "", err
	}
	return root, rel, nil
}

func (r *rootFS) Resolve(name string) (string, error) {
	if _, err := r.open(); err != nil {
		return "", err
	}
	rel, err := r.rel(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(r.dir, rel), nil
}

func (r *rootFS) Open(name string) (File, error) {
	root, rel, err := r.resolve(name)
	if err != nil {
		return nil, err
	}
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (r *rootFS) Create(name string) (File, error) {
	root, rel, err := r.resolve(name)
	if err != nil {
		return nil, err
	}
	f, err := root.Create(rel)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (r *rootFS) ReadFile(name string) ([]byte, error) {
	root, rel, err := r.resolve(name)
	if err != nil {
		return nil, err
	}
	return root.ReadFile(rel)
}

func (r *rootFS) WriteFile(name string, data []byte, perm fs.FileMode) error {
	root, rel, err := r.resolve(name)
	if err != nil {
		return err
	}
	return root.WriteFile(rel, data, perm)
}

func (r *rootFS) Stat(name string) (fs.FileInfo, error) {
	root, rel, err := r.resolve(name)
	if err != nil {
		return nil, err
	}
	return root.Stat(rel)
}

func (r *rootFS) MkdirAll(name string, perm fs.FileMode) error {
	root, rel, err := r.resolve(name)
	if err != nil {
		return err
	}
	return root.MkdirAll(rel, perm)
}

func (r *rootFS) Remove(name string) error {
	root, rel, err := r.resolve(name)
	if err != nil {
		return err
	}
	return root.Remove(rel)
}

func (r *rootFS) RemoveAll(name string) error {
	root, rel, err := r.resolve(name)
	if err != nil {
		return err
	}
	return root.RemoveAll(rel)
}

func (r *rootFS) Rename(oldpath, newpath string) error {
	root, oldRel, err := r.resolve(oldpath)
	if err != nil {
		return err
	}
	newRel, err := r.rel(newpath)
	if err != nil {
		return err
	}
	return root.Rename(oldRel, newRel)
}
