package tool

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrOutside is what a path that leaves the workspace gets, whether it
// names another directory outright, climbs out with "..", or goes out
// through a symbolic link.
var ErrOutside = errors.New("path is outside the workspace")

// Workspace is the directory the file tools are confined to. Every
// file operation goes through an os.Root, which the kernel-facing
// layer of the standard library keeps from following a name out of
// the directory, so a symbolic link inside the workspace that points
// outside it is refused rather than followed.
//
// bash is not confined by it: a shell can reach anything the user can.
// That is the policy's to decide, not the workspace's.
type Workspace struct {
	root *os.Root
	dir  string // as given, absolute and cleaned
	real string // dir with its symbolic links resolved
}

// NewWorkspace opens dir as a workspace.
func NewWorkspace(dir string) (*Workspace, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(real)
	if err != nil {
		return nil, err
	}
	return &Workspace{root: r, dir: abs, real: real}, nil
}

// Dir is the workspace's directory.
func (w *Workspace) Dir() string { return w.dir }

// Close releases the directory handle.
func (w *Workspace) Close() error { return w.root.Close() }

// Rel turns a path the model gave, absolute or relative to the
// workspace, into a name relative to its root, cleaned. A path that
// names somewhere else is ErrOutside. It checks the name alone; the
// Root's own checks catch a link that leaves.
func (w *Workspace) Rel(path string) (string, error) {
	rel, ok := NormalizePath(w.dir, w.real, path)
	if path == "" {
		return "", errors.New("path is required")
	}
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrOutside, path)
	}
	return rel, nil
}

// NormalizePath turns path, absolute or relative to dir, into a cleaned
// name relative to dir ("." for dir itself), without touching the file
// system: ./x, a/../x and the absolute path of x all come out as x. ok
// is false for a path that is empty or names somewhere outside dir.
// real, when not empty, is dir with its links resolved, which an
// absolute path may be spelled in.
func NormalizePath(dir, real, path string) (rel string, ok bool) {
	if path == "" {
		return "", false
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	path = filepath.Clean(path)
	for _, base := range []string{dir, real} {
		if base == "" {
			continue
		}
		if r, err := filepath.Rel(base, path); err == nil && local(r) {
			return r, true
		}
	}
	return "", false
}

func local(rel string) bool {
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// wrap names the path in an error the Root raised for a link that
// leaves, so the model reads the same words for every way out.
func wrap(path string, err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "path escapes from parent") {
		return fmt.Errorf("%w: %s", ErrOutside, path)
	}
	return err
}

func (w *Workspace) readFile(path string) ([]byte, string, error) {
	rel, err := w.Rel(path)
	if err != nil {
		return nil, "", err
	}
	data, err := w.root.ReadFile(rel)
	return data, rel, wrap(path, err)
}

func (w *Workspace) writeFile(path string, data []byte, mkdirs bool) (string, error) {
	rel, err := w.Rel(path)
	if err != nil {
		return "", err
	}
	if mkdirs {
		dir := filepath.Dir(rel)
		if err := w.root.MkdirAll(dir, 0o755); err != nil {
			// A link in the way reads as "file exists"; the Stat says
			// where it leads.
			if _, serr := w.root.Stat(dir); serr != nil {
				err = serr
			}
			return "", wrap(path, err)
		}
	}
	return rel, wrap(path, w.root.WriteFile(rel, data, 0o644))
}

func (w *Workspace) stat(rel string) (fs.FileInfo, error) { return w.root.Stat(rel) }

func (w *Workspace) readDir(rel string) ([]fs.DirEntry, error) {
	f, err := w.root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(-1)
}

func (w *Workspace) open(rel string) (fs.File, error) { return w.root.Open(rel) }
