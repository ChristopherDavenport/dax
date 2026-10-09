package agent

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentsmd"

	"github.com/ChristopherDavenport/dax/workspace"
)

// What AGENTS.md files go into the prompt, and from where.
//
// The chain follows the AGENTS.md convention (https://agents.md): a
// file at the repository's root and one in any directory below it, the
// nearest to the work winning. It runs from the repository's root, the
// nearest directory at or above where the session starts that holds a
// .git (a directory, or a worktree's file), down to where the session
// starts, and never above the repository. With no repository it is the
// start directory's file alone. The user's own ~/.dax/AGENTS.md, dax's
// and not the convention's, comes first, then the files the user names
// in agents_md_global for every session. Both are read on this machine
// whatever the workspace, being the user's and not the project's.
//
// The files come with the repository, and what they hold goes into the
// system prompt, which goes to the provider. A repository can ship
// AGENTS.md -> ~/.ssh/id_rsa. So the part of the chain in the
// workspace is read through the workspace's file system, which refuses
// a name that leaves it, and screened through it first, so that a file
// that is a link out of the workspace is left out and reported as
// omitted rather than failing the session. The screening reads the
// workspace, never this machine: the project may be in a container.
//
// The one exception is a session started below its repository's root
// in a workspace that is this machine's directory Dir: the files
// between the repository's root and Dir are on this machine, outside
// the workspace, and are read from it, screened on it as the workspace
// would screen them, a link that leaves Dir left out. A workspace
// elsewhere reads only its own files, and nothing comes from this
// machine but the user's file. dax-skills screens a repository's
// skills the same way.

// within reports whether p, once its links are resolved, is dir or
// below it. dir is already resolved.
func within(dir, p string) bool {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(dir, real)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func omission(part, path, reason string) agentkit.Omission {
	return agentkit.Omission{Part: part, Source: "dax", What: path, Reason: reason}
}

// agentsMDPath is where the chain is read from in the workspace: its
// root, where the session starts.
const agentsMDPath = "."

// agentsMDOptions are the options the chain is read with, over ws's
// file system, and the files the screening left out. userDir's
// AGENTS.md and the global files are the user's, read on this machine
// and not screened; a global file that is also one of the others is
// read once, in the other's place.
func agentsMDOptions(ws workspace.Workspace, dir, userDir string, global []string) (agentsmd.Options, []agentkit.Omission) {
	user := filepath.Join(userDir, "AGENTS.md")
	var host []string
	var omitted []agentkit.Omission
	start, local := hostDir(ws, dir)
	if local {
		files, refused := hostFiles(start)
		host = files
		omitted = append(omitted, refused...)
	}
	extra := []string{user}
	for _, g := range global {
		g = filepath.Clean(g)
		if g == filepath.Clean(user) || slices.Contains(extra, g) || slices.Contains(host, g) ||
			local && g == filepath.Join(start, agentsmd.DefaultNames[0]) {
			continue
		}
		extra = append(extra, g)
	}
	extra = append(extra, host...)
	fsys, refused := screened(ws)
	omitted = append(omitted, refused...)
	return agentsmd.Options{
		FS:     fsys,
		Root:   agentsMDPath,
		Extra:  extra,
		Budget: 32 << 10,
	}, omitted
}

// hostDir is dir, absolute, when ws says it is that directory on this
// machine; a workspace elsewhere has no directory here.
func hostDir(ws workspace.Workspace, dir string) (string, bool) {
	if dir == "" {
		return "", false
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	d := ws.Descriptor()
	return abs, d.Kind == workspace.KindLocal && filepath.Clean(d.Root) == abs
}

// repoRoot is the nearest directory at or above dir holding a .git,
// and false when there is none.
func repoRoot(dir string) (string, bool) {
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d, true
		}
		if filepath.Dir(d) == d {
			return "", false
		}
	}
}

// hostFiles lists the AGENTS.md files between the repository's root and
// dir, dir left out, farthest first: the part of the chain above a
// workspace that is dir on this machine. A file that is a link leaving
// dir is left out and reported.
func hostFiles(dir string) (files []string, omitted []agentkit.Omission) {
	top, ok := repoRoot(dir)
	if !ok || top == dir {
		return nil, nil
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		real = dir
	}
	var chain []string
	for d := filepath.Dir(dir); ; d = filepath.Dir(d) {
		chain = append([]string{filepath.Join(d, agentsmd.DefaultNames[0])}, chain...)
		if d == top || filepath.Dir(d) == d {
			break
		}
	}
	for _, f := range chain {
		fi, err := os.Lstat(f)
		if err != nil {
			continue
		}
		if fi.Mode()&fs.ModeSymlink != 0 && !within(real, f) {
			target, _ := filepath.EvalSymlinks(f)
			if target == "" {
				target = "a missing file"
			}
			omitted = append(omitted, omission(agentsmd.PartID, f, "symbolic link to "+target+", outside the workspace"))
			continue
		}
		files = append(files, f)
	}
	return files, omitted
}

// screened is ws's file system with the chain's files that cannot be
// read hidden, and those files reported: one that is a link out of the
// workspace, or to nothing, and one that is not a regular file, such
// as a FIFO, which a read would wait on. agentsmd then finds no file
// there. A file that becomes a link out after this is refused by the
// workspace when agentsmd reads it, which fails the session.
func screened(ws workspace.Workspace) (fs.FS, []agentkit.Omission) {
	fsys := ws.FS()
	name := path.Join(agentsMDPath, agentsmd.DefaultNames[0])
	shown := path.Join(ws.Root(), name)
	refuse := func(reason string) (fs.FS, []agentkit.Omission) {
		return hiding{fsys, name}, []agentkit.Omission{omission(agentsmd.PartID, shown, reason)}
	}
	// Lstat is Stat on a file system that cannot read links, where a
	// link out is refused here, as the read would refuse it.
	li, err := fs.Lstat(fsys, name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fsys, nil
	case errors.Is(err, workspace.ErrOutside):
		return refuse("outside the workspace")
	case err != nil:
		return refuse(err.Error())
	}
	fi, err := fs.Stat(fsys, name)
	if li.Mode()&fs.ModeSymlink != 0 && err != nil {
		target := "a missing file"
		if errors.Is(err, workspace.ErrOutside) {
			target = linkTarget(fsys, ws.Root(), name)
		}
		return refuse("symbolic link to " + target + ", outside the workspace")
	}
	switch {
	case err != nil:
		return refuse(err.Error())
	case fi.IsDir():
		return fsys, nil // not a file: agentsmd passes it over
	case !fi.Mode().IsRegular():
		return refuse("not a regular file")
	}
	return fsys, nil
}

// linkTarget is where the link name leads, as a path in the workspace's
// terms: its text, joined to the link's directory under root when
// relative.
func linkTarget(fsys fs.FS, root, name string) string {
	t, err := fs.ReadLink(fsys, name)
	if err != nil {
		return "a file outside it"
	}
	if path.IsAbs(t) {
		return path.Clean(t)
	}
	return path.Join(root, path.Dir(name), t)
}

// hiding is a file system with one name taken out: opening or stating
// it finds nothing.
type hiding struct {
	fs.FS
	name string
}

func (h hiding) Open(name string) (fs.File, error) {
	if name == h.name {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return h.FS.Open(name)
}

func (h hiding) Stat(name string) (fs.FileInfo, error) {
	if name == h.name {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
	}
	return fs.Stat(h.FS, name)
}
