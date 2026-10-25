package box

// This file is a workspace box's git guard (box.confine "workspace",
// docs/sandbox.md "Fewer questions in a box"): the files of the
// project's repository that git on the box host's machine trusts and
// runs, mounted read-only over the read-write project. A confined call
// is judged by its line, not by the program it runs; a `make`, a test or
// an archive could otherwise write a hook or a core.fsmonitor that the
// user's git would run outside the box.

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
	// GuardOn is a guarded box: its repository's hooks and config are
	// read-only in it.
	GuardOn = "on"
	// GuardNotRepo is a box asked for a guard over a project that is no
	// git repository: nothing to mount, and a `git init` in the box would
	// make a repository the user's git trusts, so nothing is claimed.
	GuardNotRepo = "none: not a git repository"
)

// GuardMounts are the paths of dir's repository a guarded box mounts
// read-only, sorted, with the guard's state: the git dir's config (and
// config.worktree, and each worktree's), its hooks (made, empty, when
// missing, so no program in the box can make them), every submodule's
// git dir under modules the same way, the effective core.hooksPath and
// .husky when they are in the project. A git dir outside the project
// needs none: the box cannot reach it. A path that resolves outside
// the project is refused, and so is a git dir with no config.
func GuardMounts(ctx context.Context, dir string) ([]string, string, error) {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, "", fmt.Errorf("guard: %w", err)
	}
	g := guard{root: root, seen: map[string]bool{}}
	gitdir, repo, err := g.gitDir()
	if err != nil {
		return nil, "", err
	}
	if !repo {
		return nil, GuardNotRepo, nil
	}
	if gitdir != "" {
		if err := g.gitDirMounts(gitdir); err != nil {
			return nil, "", err
		}
		if err := g.modules(filepath.Join(gitdir, "modules")); err != nil {
			return nil, "", err
		}
		wts, _ := filepath.Glob(filepath.Join(gitdir, "worktrees", "*", "config.worktree"))
		for _, w := range wts {
			if err := g.add(w); err != nil {
				return nil, "", err
			}
		}
	}
	hooksPath, err := g.hooksPath(ctx)
	if err != nil {
		return nil, "", err
	}
	if hooksPath != "" {
		if err := g.dir(hooksPath); err != nil {
			return nil, "", err
		}
	}
	if _, err := os.Lstat(filepath.Join(root, ".husky")); err == nil {
		if err := g.add(filepath.Join(root, ".husky")); err != nil {
			return nil, "", err
		}
	}
	sort.Strings(g.mounts)
	return g.mounts, GuardOn, nil
}

type guard struct {
	root   string
	mounts []string
	seen   map[string]bool
}

// gitDir is the project's git dir, "" when it is outside the project,
// and whether the project is a repository at all.
func (g *guard) gitDir() (string, bool, error) {
	dotgit := filepath.Join(g.root, ".git")
	fi, err := os.Lstat(dotgit)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("guard: %w", err)
	}
	target := dotgit
	if fi.Mode().IsRegular() {
		// A worktree's or a submodule's checkout: "gitdir: <path>".
		b, err := os.ReadFile(dotgit)
		if err != nil {
			return "", false, fmt.Errorf("guard: %w", err)
		}
		line, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
		if !ok {
			return "", false, fmt.Errorf("guard: %s: not a gitdir file", dotgit)
		}
		target = strings.TrimSpace(line)
		if !filepath.IsAbs(target) {
			target = filepath.Join(g.root, target)
		}
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", false, fmt.Errorf("guard: the git dir %s: %w", target, err)
	}
	if !g.inside(resolved) {
		// The box mounts the project alone, so it cannot reach it.
		return "", true, nil
	}
	return resolved, true, nil
}

// gitDirMounts guards one git dir: its config, which must be there,
// config.worktree when there is one, and its hooks, made when missing.
func (g *guard) gitDirMounts(gitdir string) error {
	config := filepath.Join(gitdir, "config")
	if _, err := os.Stat(config); err != nil {
		return fmt.Errorf("guard: the git dir %s has no config to hold read-only: %w", gitdir, err)
	}
	if err := g.add(config); err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(gitdir, "config.worktree")); err == nil {
		if err := g.add(filepath.Join(gitdir, "config.worktree")); err != nil {
			return err
		}
	}
	return g.dir(filepath.Join(gitdir, "hooks"))
}

// modules guards every submodule's git dir under dir, nested ones too:
// a directory with a HEAD and a config.
func (g *guard) modules(dir string) error {
	if _, err := os.Lstat(dir); err != nil {
		return nil
	}
	var gitdirs []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if _, err := os.Stat(filepath.Join(p, "HEAD")); err != nil {
			return nil
		}
		if _, err := os.Stat(filepath.Join(p, "config")); err != nil {
			return nil
		}
		gitdirs = append(gitdirs, p)
		return nil
	})
	if err != nil {
		return fmt.Errorf("guard: %w", err)
	}
	for _, gd := range gitdirs {
		if err := g.gitDirMounts(gd); err != nil {
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

// dir guards a directory, made, empty, when missing.
func (g *guard) dir(p string) error {
	if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(p, 0o755); err != nil {
			return fmt.Errorf("guard: %w", err)
		}
	}
	return g.add(p)
}

// add guards p, which must resolve inside the project.
func (g *guard) add(p string) error {
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return fmt.Errorf("guard: %w", err)
	}
	if !g.inside(resolved) {
		return fmt.Errorf("guard: %s leads out of the project, to %s; it cannot be held read-only in the box", p, resolved)
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
