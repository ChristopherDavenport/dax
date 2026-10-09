package tool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ChristopherDavenport/dax/workspace"
)

// ErrOutside is what a path that leaves the workspace gets, whether it
// names another directory outright, climbs out with "..", or goes out
// through a symbolic link.
var ErrOutside = workspace.ErrOutside

// Files is the file tools' view of a workspace.Workspace: the paths the
// model writes, absolute or relative to the workspace's root, turned
// into the workspace's names, and the lock that keeps a write from
// landing between another's read and write. The workspace does the
// rest, so the same tools act on this machine's directory
// (workspace.Local), a container or a remote runtime alike.
//
// bash is not confined by it: a shell can reach anything the
// workspace's processes can. That is the policy's to decide.
//
// A session makes one Files for its workspace, and every tool that
// writes shares it, so the lock is one lock.
type Files struct {
	ws   workspace.Workspace
	dir  string // the root as the model sees it
	real string // the root with its links resolved, when the workspace can tell
	// writes is held across each write, and across an edit's read and
	// write, so two calls running at once (sub-agents in one batch, or
	// parallel calls of one agent) cannot lose an edit to a write
	// based on an older read.
	writes sync.Mutex
}

// NewFiles is the tools' view of ws. The workspace stays its owner's to
// close.
func NewFiles(ws workspace.Workspace) *Files {
	f := &Files{ws: ws, dir: filepath.Clean(ws.Root())}
	if r, ok := ws.(interface{ Real() string }); ok {
		f.real = r.Real()
	}
	return f
}

// Workspace is the workspace the files are in.
func (w *Files) Workspace() workspace.Workspace { return w.ws }

// Dir is the workspace's root, as the model sees it.
func (w *Files) Dir() string { return w.dir }

// Rel turns a path the model gave, absolute or relative to the
// workspace, into a name relative to its root, cleaned. A path that
// names somewhere else is ErrOutside. It checks the name alone; the
// Root's own checks catch a link that leaves.
func (w *Files) Rel(path string) (string, error) {
	rel, ok := normalizePath(w.dir, w.real, path)
	if path == "" {
		return "", errors.New("path is required")
	}
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrOutside, path)
	}
	return rel, nil
}

// Resolve is the name, relative to the workspace's root, that path
// leads to with every link on its way followed, read through the
// workspace's file system, as the policy's checks follow them. A path
// or a link that leads out of the workspace is ErrOutside; a workspace
// whose file system cannot read links, or a chain of links too long to
// follow, is ErrLinksUnknown, so a caller that must know where a name
// leads fails toward refusing. A name that does not exist is judged by
// where its directory leads. An extension that offers a project's
// files through a tool other than the file tools (dax-skills) uses it
// to keep a link from reaching past what it offers.
func (w *Files) Resolve(path string) (string, error) {
	rel, err := w.Rel(path)
	if err != nil {
		return "", err
	}
	real, r := w.view().resolve(rel)
	switch r {
	case outside:
		return "", fmt.Errorf("%w: %s", ErrOutside, path)
	case unknown:
		return "", fmt.Errorf("%w: %s", ErrLinksUnknown, path)
	}
	return filepath.Clean(real), nil
}

// ErrLinksUnknown is Resolve's error where the workspace cannot say
// where a link leads.
var ErrLinksUnknown = errors.New("the workspace cannot say where its links lead")

// normalizePath turns path, absolute or relative to dir, into a cleaned
// name relative to dir ("." for dir itself), without touching the file
// system: ./x, a/../x and the absolute path of x all come out as x. ok
// is false for a path that is empty or names somewhere outside dir.
// real, when not empty, is dir with its links resolved, which an
// absolute path may be spelled in.
func normalizePath(dir, real, path string) (rel string, ok bool) {
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

// wrap names the path the model wrote in an error the workspace raised
// for a name that leaves, so the model reads the same words for every
// way out.
func wrap(path string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrOutside) || strings.Contains(err.Error(), "path escapes from parent") {
		return fmt.Errorf("%w: %s", ErrOutside, path)
	}
	return err
}

// ReadFile, WriteFile, Update, Stat and ReadDir are for an extension's
// tools: each takes a path as the model wrote it, absolute or relative
// to the workspace, and goes through the workspace, so a name that
// leaves is ErrOutside, as it is for dax's own file tools. None blocks on a FIFO
// or a device, and the writes take the lock dax's write and edit hold,
// so a write of one tool does not land between another's read and
// write.

// ReadFile reads a regular file no larger than max bytes; zero or less
// is DefaultMaxRead.
func (w *Files) ReadFile(path string, max int64) ([]byte, error) {
	if max <= 0 {
		max = DefaultMaxRead
	}
	return w.readFileMax(path, max)
}

// WriteFile writes data to a regular file, creating it and the
// directories above it, and returns its name relative to the workspace.
func (w *Files) WriteFile(path string, data []byte) (string, error) {
	w.writes.Lock()
	defer w.writes.Unlock()
	return w.writeFile(path, data, true)
}

// Update reads a regular file no larger than max bytes (zero or less is
// DefaultMaxRead), and writes what fn makes of its content, holding the
// write lock across both, as the edit tool does. An error from fn
// leaves the file as it was. It returns the file's name relative to the
// workspace.
func (w *Files) Update(path string, max int64, fn func(old []byte) ([]byte, error)) (string, error) {
	if max <= 0 {
		max = DefaultMaxRead
	}
	w.writes.Lock()
	defer w.writes.Unlock()
	old, err := w.readFileMax(path, max)
	if err != nil {
		return "", err
	}
	data, err := fn(old)
	if err != nil {
		return "", err
	}
	return w.writeFile(path, data, false)
}

// Stat describes a file, following a link only while it stays inside.
func (w *Files) Stat(path string) (fs.FileInfo, error) {
	rel, err := w.Rel(path)
	if err != nil {
		return nil, err
	}
	fi, err := w.stat(rel)
	return fi, wrap(path, err)
}

// ReadDir lists a directory, in the order the system gives.
func (w *Files) ReadDir(path string) ([]fs.DirEntry, error) {
	rel, err := w.Rel(path)
	if err != nil {
		return nil, err
	}
	entries, err := w.readDir(rel)
	return entries, wrap(path, err)
}

// openRegular opens a file for reading and says how big it is. A
// directory, a FIFO or a device is an error: opening a FIFO blocks, and
// nothing a file tool does with one is useful.
func (w *Files) openRegular(path string) (f fs.File, rel string, size int64, err error) {
	rel, err = w.Rel(path)
	if err != nil {
		return nil, "", 0, err
	}
	file, err := w.open(rel)
	if err != nil {
		return nil, "", 0, wrap(path, err)
	}
	fi, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, "", 0, err
	}
	if !fi.Mode().IsRegular() {
		file.Close()
		return nil, "", 0, fmt.Errorf("%s is not a regular file", rel)
	}
	return file, rel, fi.Size(), nil
}

// readFileMax reads a whole file no larger than max bytes.
func (w *Files) readFileMax(path string, max int64) ([]byte, error) {
	f, _, size, err := w.openRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if size > max {
		return nil, fmt.Errorf("%s is %d bytes, over the %d-byte limit for this tool", path, size, max)
	}
	return io.ReadAll(io.LimitReader(f, max+1))
}

func (w *Files) writeFile(path string, data []byte, _ bool) (string, error) {
	rel, err := w.Rel(path)
	if err != nil {
		return "", err
	}
	// The workspace creates the directories above the file, and refuses
	// a FIFO or a device without blocking on it, which would otherwise
	// hang the write and the lock with it. An edit's file exists, so the
	// directories are there already.
	if err := w.ws.WriteFile(context.Background(), name(rel), data, 0o644); err != nil {
		return "", wrap(path, err)
	}
	return rel, nil
}

// name is a workspace-relative path as the workspace's FS names it.
func name(rel string) string { return filepath.ToSlash(rel) }

func (w *Files) stat(rel string) (fs.FileInfo, error) { return fs.Stat(w.ws.FS(), name(rel)) }

// readDir lists a directory; the workspace opens it as one, without
// blocking, so a FIFO or a device in its place is an error at once.
func (w *Files) readDir(rel string) ([]fs.DirEntry, error) { return fs.ReadDir(w.ws.FS(), name(rel)) }

// open opens a name for reading without blocking: a FIFO would
// otherwise wait for a writer that never comes. The caller checks what
// it opened is a regular file.
func (w *Files) open(rel string) (fs.File, error) { return w.ws.FS().Open(name(rel)) }
