package tool

import (
	"errors"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
)

// view is the workspace as the policy's checks read it. Paths are
// absolute in the workspace's own namespace, the one its processes see
// (Root, and Real for a workspace that can say where its root's links
// lead), and every look at a file goes through the workspace's FS, so
// a check of a command that runs in a container reads the container's
// files and not this machine's.
type view struct {
	dir, real string
	fsys      fs.FS
	// links is fsys when it can tell a link from what it leads to; nil
	// when it cannot, and then a check that must follow links cannot,
	// and fails toward asking.
	links fs.ReadLinkFS
}

// view is w as the checks read it.
func (w *Files) view() view {
	v := view{dir: w.dir, real: w.real, fsys: w.ws.FS()}
	v.links, _ = v.fsys.(fs.ReadLinkFS)
	return v
}

// reach is where a name's links lead.
type reach int

const (
	// inside: the name, its links followed, is in the workspace.
	inside reach = iota
	// outside: a link on its way leads out of the workspace.
	outside
	// unknown: the workspace cannot say where its links lead.
	unknown
)

// maxHops bounds the links one resolution follows.
const maxHops = 40

// rel is the workspace-relative name of p, absolute in the workspace's
// namespace or relative to its root; ok is false for one outside.
func (v view) rel(p string) (string, bool) { return normalizePath(v.dir, v.real, p) }

// resolve follows the links of the longest prefix of the workspace name
// rel that exists, inside the workspace, and puts the rest back, so a
// name that does not exist yet is judged by where its directory really
// is. It reads links only through the workspace: one that leads out is
// outside, and with an FS that cannot read links the answer is unknown.
func (v view) resolve(rel string) (string, reach) {
	if v.links == nil {
		return "", unknown
	}
	parts := components(rel)
	cur := "."
	for hops := 0; len(parts) > 0; {
		next := path.Join(cur, parts[0])
		fi, err := v.links.Lstat(next)
		switch {
		case errors.Is(err, ErrOutside):
			return "", outside
		case err != nil:
			// Not there (or not a directory on the way): the rest is
			// judged by where cur is, as a file not written yet is.
			return filepath.FromSlash(path.Join(append([]string{cur}, parts...)...)), inside
		case fi.Mode()&fs.ModeSymlink == 0:
			cur, parts = next, parts[1:]
			continue
		}
		if hops++; hops > maxHops {
			return "", unknown
		}
		target, err := v.links.ReadLink(next)
		if err != nil {
			return "", unknown
		}
		var trel string
		if path.IsAbs(target) || filepath.IsAbs(target) {
			r, ok := v.rel(target)
			if !ok {
				return "", outside
			}
			trel = filepath.ToSlash(r)
		} else {
			trel = path.Join(cur, target)
			if trel == ".." || strings.HasPrefix(trel, "../") {
				return "", outside
			}
		}
		parts, cur = append(components(trel), parts[1:]...), "."
	}
	return filepath.FromSlash(cur), inside
}

// components are rel's names, without "." and empty ones.
func components(rel string) []string {
	var out []string
	for _, c := range strings.Split(filepath.ToSlash(rel), "/") {
		if c != "" && c != "." {
			out = append(out, c)
		}
	}
	return out
}

// inside reports whether arg, an argument of a command run in cwd (the
// root or a directory below it after a cd), names something in the
// workspace with its links followed. A path that climbs with "..", a ~
// and anything whose links cannot be read are not.
func (v view) inside(arg, cwd string) bool {
	if strings.HasPrefix(arg, "~") {
		return false
	}
	for _, c := range strings.Split(arg, "/") {
		if c == ".." {
			return false
		}
	}
	p := arg
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	rel, ok := v.rel(filepath.Clean(p))
	if !ok {
		return false
	}
	_, r := v.resolve(rel)
	return r == inside
}

// stat describes p, absolute in the workspace's namespace, following
// links that stay inside.
func (v view) stat(p string) (fs.FileInfo, error) {
	rel, ok := v.rel(p)
	if !ok {
		return nil, &fs.PathError{Op: "stat", Path: p, Err: ErrOutside}
	}
	return fs.Stat(v.fsys, name(rel))
}

// sub is the workspace's files from cwd down, for a glob run there.
func (v view) sub(cwd string) (fs.FS, error) {
	rel, ok := v.rel(cwd)
	if !ok {
		return nil, ErrOutside
	}
	if rel == "." {
		return v.fsys, nil
	}
	return fs.Sub(v.fsys, name(rel))
}

// strictFS records the first error a read of it met other than a name
// that is not there. fs.Glob passes over a directory it cannot read,
// which over a confined FS is a link out of the workspace that bash
// itself would follow; a glob checked through strictFS is refused for
// it instead.
type strictFS struct {
	fs.FS
	err *error
}

func (s strictFS) note(err error) error {
	if err != nil && !errors.Is(err, fs.ErrNotExist) && *s.err == nil {
		*s.err = err
	}
	return err
}

func (s strictFS) Open(name string) (fs.File, error) {
	f, err := s.FS.Open(name)
	return f, s.note(err)
}

func (s strictFS) ReadDir(name string) ([]fs.DirEntry, error) {
	es, err := fs.ReadDir(s.FS, name)
	return es, s.note(err)
}

func (s strictFS) Stat(name string) (fs.FileInfo, error) {
	fi, err := fs.Stat(s.FS, name)
	return fi, s.note(err)
}
