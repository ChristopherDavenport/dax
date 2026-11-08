package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing/fstest"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentsmd"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/agentworkspace/remote"

	"github.com/ChristopherDavenport/dax/internal/config"
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

// noRead is the reason a served workspace's project files are left out
// when its grant holds its servers alone.
const noRead = "the workspace grants no read"

// serversOnly reports whether ws is a workspace the session can only
// start servers in (config.ServersOnly): a read it refuses
// (remote.ErrForbidden) then leaves nothing for a project's rule to
// tighten.
func serversOnly(ws workspace.Workspace) bool {
	return config.ServersOnly(ws)
}

// agentsMDPath is where the chain is read from in the workspace: its
// root, where the session starts.
const agentsMDPath = "."

// agentsMDBudget is the chain's budget: n, or the default when n is 0
// or less.
func agentsMDBudget(n int64) int64 {
	if n > 0 {
		return n
	}
	return config.DefaultAgentsMDBudget
}

// ownBytes are the bytes of the person's own files (personal) among
// the chain's files: the share a project's bound leaves alone.
func ownBytes(files []agentsmd.File, personal []string) int64 {
	var n int64
	for _, f := range files {
		if mine(personal, f.Path) {
			n += int64(len(f.Content))
		}
	}
	return n
}

// ownOmitted reports whether the chain left out a file of the
// person's own (personal).
func ownOmitted(omitted []agentsmd.Omitted, personal []string) bool {
	for _, o := range omitted {
		if mine(personal, o.Path) {
			return true
		}
	}
	return false
}

// agentsMDOptions are the options the chain is read with, within
// budget bytes, over ws's file system, the person's own files among
// them, and the files the screening left out. userDir's AGENTS.md and the global files are the
// user's, read on this machine and not screened; a global file that is
// also one of the others is read once, in the other's place, and is
// that other's: a project's file a global names is the project's, not
// the person's own (caller.go).
func agentsMDOptions(ws workspace.Workspace, dir, userDir string, global []string, budget int64) (agentsmd.Options, []string, []agentkit.Omission) {
	var host []string
	var omitted []agentkit.Omission
	start, local := hostDir(ws, dir)
	if local {
		files, refused := hostFiles(start)
		host = files
		omitted = append(omitted, refused...)
	}
	// An empty userDir leaves the user's own file out (dax -bare).
	var extra []string
	if userDir != "" {
		extra = append(extra, filepath.Join(userDir, "AGENTS.md"))
	}
	for _, g := range global {
		g = filepath.Clean(g)
		if slices.Contains(extra, g) || slices.Contains(host, g) ||
			local && g == filepath.Join(start, agentsmd.DefaultNames[0]) {
			continue
		}
		extra = append(extra, g)
	}
	personal := slices.Clone(extra)
	extra = append(extra, host...)
	fsys, refused := screened(ws)
	omitted = append(omitted, refused...)
	return agentsmd.Options{
		FS:     fsys,
		Root:   agentsMDPath,
		Extra:  extra,
		Budget: budget,
		// The first file over the budget is cut at a line, with
		// agentsmd's marker saying how much went, rather than left out
		// whole: its first part still reaches the model. It never adds
		// an error.
		Truncate: true,
	}, personal, omitted
}

// cutKept is the bytes of the file res cut under agentsmd's Truncate
// that reach the model, ahead of the marker, and false when no file
// was cut. The marker is the one agentsmd documents; the line before
// it ends the part kept should its wording ever change.
func cutKept(res agentsmd.Result) (int64, bool) {
	for _, o := range res.Omitted {
		if o.Reason != agentsmd.Truncated {
			continue
		}
		for _, f := range res.Files {
			if f.Path != o.Path {
				continue
			}
			marker := fmt.Sprintf("[agentsmd: %d bytes of %s left out over the budget]\n", o.Size, o.Path)
			if strings.HasSuffix(f.Content, marker) {
				return int64(len(f.Content) - len(marker)), true
			}
			return int64(strings.LastIndexByte(strings.TrimSuffix(f.Content, "\n"), '\n') + 1), true
		}
	}
	return 0, false
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
	case errors.Is(err, remote.ErrForbidden) && serversOnly(ws):
		// A served workspace the session may not read, and so not act
		// in (granted its servers alone), has no chain of its own to
		// read: the chain is the user's files alone. One that may run
		// commands or write there but not read is refused below, and
		// the session with it.
		return fstest.MapFS{}, []agentkit.Omission{omission(agentsmd.PartID, shown, noRead)}
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
