package box

// This file is a workspace box's git guard (box.confine "workspace",
// docs/sandbox.md "The git guard"): the project's repository, its .git
// whole, mounted read-only over the read-write project. A confined call
// is judged by its line, not by the program it runs; a `make`, a test or
// an archive could otherwise write a hook, a config or a commondir that
// the user's git would trust outside the box, or remove the history.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// The guard's states, as the workspace's envelope reports them.
const (
	// GuardOn is a guarded box: its repository's .git is read-only in
	// it, so nothing a program there runs can change what git on the
	// user's machine trusts (hooks, config, commondir) or its history.
	GuardOn = "on: .git read-only"
	// GuardNotRepo is a box asked for a guard over a project that is no
	// git repository: nothing to mount, and a `git init` in the box would
	// make a repository the user's git trusts, so nothing is claimed.
	GuardNotRepo = "none: not a git repository"
)

// GuardMounts are the paths of dir's repository a guarded box mounts
// read-only, sorted, with the guard's state. Read-only, git on the box
// host's machine trusts nothing the box wrote, whatever program a
// confined call runs:
//
//   - the project's .git: a directory, mounted whole, so its hooks,
//     config and commondir cannot change and it cannot be renamed or
//     removed (it is a mount point); or a .git file, mounted itself,
//     with its git dir when that is in the project (one outside the
//     project the box cannot reach);
//   - every checked-out submodule's .git, nested ones too, as git lists
//     them when the box is made: a rewritten gitdir line would send git
//     to a directory the box wrote;
//   - the effective core.hooksPath when it is in the project, and
//     .husky.
//
// Nothing is made or changed in the repository. A guard path that is a
// symbolic link, or resolves out of the project, refuses the guard,
// since the link itself would stay writable; so does a core.hooksPath
// in the project that is not there yet, which a program could make.
func GuardMounts(ctx context.Context, dir string) ([]string, string, error) {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, "", fmt.Errorf("guard: %w", err)
	}
	g := guard{root: root, seen: map[string]bool{}}
	dotgit := filepath.Join(root, ".git")
	if _, err := os.Lstat(dotgit); errors.Is(err, fs.ErrNotExist) {
		return nil, GuardNotRepo, nil
	} else if err != nil {
		return nil, "", fmt.Errorf("guard: %w", err)
	}
	if err := g.dotGit(dotgit); err != nil {
		return nil, "", err
	}
	if err := g.nested(ctx); err != nil {
		return nil, "", err
	}
	hooksPath, err := g.hooksPath(ctx)
	if err != nil {
		return nil, "", err
	}
	if hooksPath != "" {
		if _, err := os.Lstat(hooksPath); errors.Is(err, fs.ErrNotExist) {
			return nil, "", fmt.Errorf("guard: core.hooksPath %s is in the project but not there yet, and a program in the box could make it; create it (or unset core.hooksPath) before boxing", hooksPath)
		}
		if err := g.add(hooksPath); err != nil {
			return nil, "", err
		}
	}
	if _, err := os.Lstat(filepath.Join(root, ".husky")); err == nil {
		if err := g.add(filepath.Join(root, ".husky")); err != nil {
			return nil, "", err
		}
	}
	return g.result(), GuardOn, nil
}

type guard struct {
	root   string
	mounts []string
	seen   map[string]bool
}

// dotGit guards one .git: a directory whole; a file itself, and its
// git dir when that is in the project.
func (g *guard) dotGit(dotgit string) error {
	fi, err := os.Lstat(dotgit)
	if err != nil {
		return fmt.Errorf("guard: %w", err)
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("guard: %s is a symbolic link, which would stay writable in the box; it cannot be held read-only", dotgit)
	case fi.IsDir():
		return g.add(dotgit)
	case !fi.Mode().IsRegular():
		return fmt.Errorf("guard: %s is neither a directory nor a gitdir file", dotgit)
	}
	// A worktree's or a submodule's checkout: "gitdir: <path>".
	if err := g.add(dotgit); err != nil {
		return err
	}
	b, err := os.ReadFile(dotgit)
	if err != nil {
		return fmt.Errorf("guard: %w", err)
	}
	line, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return fmt.Errorf("guard: %s: not a gitdir file", dotgit)
	}
	target := strings.TrimSpace(line)
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(dotgit), target)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return fmt.Errorf("guard: the git dir %s: %w", target, err)
	}
	if !g.inside(resolved) {
		// The box mounts the project alone, so it cannot reach it.
		return nil
	}
	return g.add(resolved)
}

// nested guards each submodule's .git, nested submodules too, as git
// lists them when the box is made: a file whose gitdir line, rewritten,
// would send git (a `git status` in the superproject recurses into its
// submodules) to a directory the box wrote. A submodule not checked out
// has none to guard.
func (g *guard) nested(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "git", "-C", g.root, "submodule", "status", "--recursive")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("guard: git submodule status: %s", strings.TrimSpace(stderr.String()))
	}
	for _, line := range strings.Split(string(out), "\n") {
		// "<state><sha> <path>[ (<describe>)]", state one of ' ', '-',
		// '+', 'U'; '-' is not checked out.
		if len(line) < 2 || line[0] == '-' {
			continue
		}
		fields := strings.Fields(line[1:])
		if len(fields) < 2 {
			continue
		}
		dotgit := filepath.Join(g.root, filepath.FromSlash(fields[1]), ".git")
		if _, err := os.Lstat(dotgit); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if !g.insideLexical(dotgit) {
			return fmt.Errorf("guard: the submodule %s leads out of the project", fields[1])
		}
		if err := g.dotGit(dotgit); err != nil {
			return err
		}
	}
	return nil
}

// hooksPath is the repository's effective core.hooksPath, the box
// host's own global config included, when it is in the project; ""
// when it is unset or outside it.
func (g *guard) hooksPath(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", g.root, "config", "--type=path", "--get", "core.hooksPath")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return "", nil // unset
		}
		return "", fmt.Errorf("guard: git config core.hooksPath: %s", strings.TrimSpace(stderr.String()))
	}
	p := strings.TrimSpace(string(out))
	if p == "" {
		return "", nil
	}
	if !filepath.IsAbs(p) {
		// Relative to the worktree's root, where hooks run.
		p = filepath.Join(g.root, p)
	}
	p = filepath.Clean(p)
	if p == g.root {
		return "", fmt.Errorf("guard: core.hooksPath is the project's root (%s); its hooks cannot be held read-only apart from the project", p)
	}
	if !g.insideLexical(p) {
		return "", nil
	}
	return p, nil
}

// add guards p, which must be no link and resolve to itself inside the
// project: a link would stay writable in the box, and could be pointed
// elsewhere.
func (g *guard) add(p string) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return fmt.Errorf("guard: %w", err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("guard: %s is a symbolic link, which would stay writable in the box; it cannot be held read-only", p)
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return fmt.Errorf("guard: %w", err)
	}
	if !g.inside(resolved) {
		return fmt.Errorf("guard: %s leads out of the project, to %s; it cannot be held read-only in the box", p, resolved)
	}
	if resolved != filepath.Clean(p) {
		return fmt.Errorf("guard: %s is reached through a symbolic link (it is %s), which would stay writable in the box", p, resolved)
	}
	if strings.Contains(resolved, ":") {
		return fmt.Errorf("guard: %s: a path with a colon cannot be mounted", resolved)
	}
	if !g.seen[resolved] {
		g.seen[resolved] = true
		g.mounts = append(g.mounts, resolved)
	}
	return nil
}

// result is the mounts sorted, less any under another: a git dir in
// the project's .git is read-only with it.
func (g *guard) result() []string {
	sort.Strings(g.mounts)
	var out []string
	for _, m := range g.mounts {
		if len(out) > 0 {
			last := out[len(out)-1]
			if strings.HasPrefix(m, last+string(filepath.Separator)) {
				continue
			}
		}
		out = append(out, m)
	}
	return out
}

func (g *guard) inside(p string) bool {
	rel, err := filepath.Rel(g.root, p)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// insideLexical is inside for a path that may not exist yet: its
// nearest existing parent resolved, then the rest.
func (g *guard) insideLexical(p string) bool {
	parent, rest := p, ""
	for {
		if _, err := os.Lstat(parent); err == nil {
			break
		}
		next := filepath.Dir(parent)
		if next == parent {
			return false
		}
		rest = filepath.Join(filepath.Base(parent), rest)
		parent = next
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return false
	}
	return g.inside(filepath.Join(resolved, rest))
}

// guardLabel is what a box's guard was made with, for its spec label:
// "" for none, else the state and the mounts.
func guardLabel(s Spec) string {
	if !s.Guard {
		return ""
	}
	return s.GuardState + "\x00" + strings.Join(s.GuardMounts, "\x00")
}

// guardTag is a box's labelGuard value: "none" without a guard, else a
// short hash of guardLabel.
func guardTag(s Spec) string {
	g := guardLabel(s)
	if g == "" {
		return "none"
	}
	sum := sha256.Sum256([]byte(g))
	return hex.EncodeToString(sum[:8])
}
