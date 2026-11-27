package tool

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// GitMoves names where a command line's git commands that can move a
// checkout ran, and what they made: -C and cd move it, a worktree add,
// move or remove and a clone name a second directory, a read-only git
// command and a line with no git name nothing, a line the parser
// cannot read that names git asks for every checkout, and a directory
// outside the workspace is left out.
func TestGitMoves(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.MkdirAll(filepath.Join(dir, "repo"), 0o755)
	f := newWS(t, dir)
	for _, tc := range []struct {
		cmd  string
		dirs []string
		all  bool
	}{
		{"git commit -qm x", []string{"."}, false},
		{"git -C sub commit -m x", []string{"sub"}, false},
		{"cd sub && git push origin HEAD", []string{"sub"}, false},
		{"git -c user.name=n -C repo switch -c fix", []string{"repo"}, false},
		{"git worktree add -b fix .worktrees/fix origin/main", []string{".", ".worktrees/fix"}, false},
		{"git -C repo worktree add ../wt", []string{"repo", "wt"}, false},
		{"git worktree remove .worktrees/fix", []string{".", ".worktrees/fix"}, false},
		{"git clone -q src dst", []string{".", "dst"}, false},
		{"git status && git log --oneline", nil, false},
		{"git diff | head", nil, false},
		{"make check", nil, false},
		{"git -C /elsewhere commit -m x", nil, false},
		{"git commit -m x; git push", nil, true},
		{"echo $(git rev-parse HEAD)", nil, true},
		{"ls; echo digit", nil, false},
		{"/usr/bin/git commit -am 'x'; true", nil, true},
	} {
		dirs, all := GitMoves(tc.cmd, f)
		if !slices.Equal(dirs, tc.dirs) || all != tc.all {
			t.Errorf("%q: %v all=%v, want %v all=%v", tc.cmd, dirs, all, tc.dirs, tc.all)
		}
	}
}

// ReadCheckout reads where a checkout is and the commit, branch and
// upstream it is on, through the workspace, and whether its tree is
// dirty: a worktree shares its repository's common directory, a
// detached HEAD has no branch, a push moves the upstream, a colocated
// jj repository and one whose config runs a program are read only as
// far as where they are, a directory in no repository is nothing, one
// removed is ErrGone, and a workspace with no git is ErrNoGit.
func TestReadCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	ctx := context.Background()
	dir := t.TempDir()
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitRun(t, filepath.Dir(remote), "init", "-q", "--bare", "-b", "main", remote)
	gitRun(t, dir, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644)
	gitRun(t, dir, "add", "a.txt")
	gitRun(t, dir, "commit", "-q", "-m", "one")
	gitRun(t, dir, "remote", "add", "origin", remote)
	gitRun(t, dir, "push", "-q", "-u", "origin", "main")
	f := newWS(t, dir)
	head := func(d string) string {
		out, err := exec.Command("git", "-C", d, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	real := func(p string) string {
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	c, err := ReadCheckout(ctx, f, ".")
	if err != nil || !c.Repo || c.Top != real(dir) || c.CommonDir != real(filepath.Join(dir, ".git")) ||
		c.Revision != head(dir) || c.Branch != "main" || c.Dirty || c.Upstream != "origin/main" || c.UpstreamRevision != head(dir) {
		t.Fatalf("root: %+v, %v", c, err)
	}
	if strings.Contains(c.Upstream, remote) {
		t.Errorf("the upstream names the remote's URL: %q", c.Upstream)
	}

	// A worktree, dirty, on a branch of its own with no upstream.
	gitRun(t, dir, "worktree", "add", "-q", "-b", "fix", "wt")
	os.WriteFile(filepath.Join(dir, "wt", "b.txt"), []byte("b\n"), 0o644)
	w, err := ReadCheckout(ctx, f, "wt")
	if err != nil || w.Top != real(filepath.Join(dir, "wt")) || w.CommonDir != c.CommonDir || w.Branch != "fix" || !w.Dirty || w.Upstream != "" {
		t.Fatalf("worktree: %+v, %v", w, err)
	}

	// A commit there and a push of it move the revision and the upstream.
	gitRun(t, filepath.Join(dir, "wt"), "add", "b.txt")
	gitRun(t, filepath.Join(dir, "wt"), "commit", "-q", "-m", "two")
	gitRun(t, filepath.Join(dir, "wt"), "push", "-q", "-u", "origin", "fix")
	if w, err = ReadCheckout(ctx, f, "wt"); err != nil || w.Revision != head(filepath.Join(dir, "wt")) || w.Dirty || w.Upstream != "origin/fix" || w.UpstreamRevision != w.Revision {
		t.Fatalf("worktree after a push: %+v, %v", w, err)
	}

	// Detached: no branch.
	gitRun(t, filepath.Join(dir, "wt"), "checkout", "-q", "--detach")
	if w, err = ReadCheckout(ctx, f, "wt"); err != nil || w.Branch != "" || w.Revision == "" {
		t.Fatalf("detached: %+v, %v", w, err)
	}

	// Removed: gone.
	gitRun(t, dir, "worktree", "remove", "--force", "wt")
	if _, err := ReadCheckout(ctx, f, "wt"); !errors.Is(err, ErrGone) {
		t.Fatalf("removed worktree: %v, want ErrGone", err)
	}

	// Colocated jj: where it is, not its state.
	os.Mkdir(filepath.Join(dir, ".jj"), 0o755)
	if j, err := ReadCheckout(ctx, f, "."); err != nil || !j.JJ || j.Top == "" || j.Revision != "" || j.Branch != "" {
		t.Fatalf("jj: %+v, %v", j, err)
	}
	os.Remove(filepath.Join(dir, ".jj"))

	// A filter the repository sets: withheld, and the filter never runs.
	mark := filepath.Join(t.TempDir(), "ran")
	gitRun(t, dir, "config", "filter.x.clean", "touch "+mark+"; cat")
	os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("*.txt filter=x\n"), 0o644)
	if g, err := ReadCheckout(ctx, f, "."); err != nil || g.Withheld == "" || g.Top == "" || g.Revision != "" {
		t.Fatalf("a filter: %+v, %v", g, err)
	}
	if _, err := os.Stat(mark); err == nil {
		t.Fatal("the repository's filter ran")
	}

	// No repository; outside; no git.
	if n, err := ReadCheckout(ctx, newWS(t, t.TempDir()), "."); err != nil || n.Repo {
		t.Fatalf("no repository: %+v, %v", n, err)
	}
	if _, err := ReadCheckout(ctx, f, "/elsewhere"); !errors.Is(err, ErrOutside) {
		t.Fatalf("outside: %v, want ErrOutside", err)
	}
	bare := newWSEnv(t, dir, []string{"PATH=" + t.TempDir()})
	if _, err := ReadCheckout(ctx, bare, "."); !errors.Is(err, ErrNoGit) {
		t.Fatalf("no git: %v, want ErrNoGit", err)
	}
}

// GitDiffIn reads a checkout below the root at its own top: a
// worktree's change against its own commit.
func TestGitDiffInAWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644)
	gitRun(t, dir, "add", "a.txt")
	gitRun(t, dir, "commit", "-q", "-m", "one")
	gitRun(t, dir, "worktree", "add", "-q", "-b", "fix", "wt")
	os.WriteFile(filepath.Join(dir, "wt", "a.txt"), []byte("changed\n"), 0o644)
	f := newWS(t, dir)
	c, err := GitDiffIn(context.Background(), f, "wt", "", 1<<20)
	if err != nil || !c.Repo || !c.Commit || c.Size == 0 || !strings.Contains(string(c.Data), "+changed") {
		t.Fatalf("worktree diff: %+v %q, %v", c, c.Data, err)
	}
	root, err := GitDiff(context.Background(), f, "", 1<<20)
	if err != nil || strings.Contains(string(root.Data), "+changed") {
		t.Fatalf("the root's diff holds the worktree's change: %q, %v", root.Data, err)
	}
}

// A linked worktree is read only when its gitfile is git's own link: a
// .git file naming another repository's git directory, or a worktree's
// that does not lead back to it, stays withheld, and so does a real
// worktree whose repository's config runs a program, which never runs.
func TestReadCheckoutGitfiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	ctx := context.Background()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644)
	gitRun(t, dir, "add", "a.txt")
	gitRun(t, dir, "commit", "-q", "-m", "one")
	gitRun(t, dir, "worktree", "add", "-q", "-b", "fix", "wt")
	f := newWS(t, dir)
	if w, err := ReadCheckout(ctx, f, "wt"); err != nil || w.Withheld != "" || w.Branch != "fix" {
		t.Fatalf("a worktree git made: %+v, %v", w, err)
	}

	// A gitfile naming a repository elsewhere, outside the workspace.
	other := t.TempDir()
	gitRun(t, other, "init", "-q", "-b", "main")
	os.MkdirAll(filepath.Join(dir, "forged"), 0o755)
	os.WriteFile(filepath.Join(dir, "forged", ".git"), []byte("gitdir: "+filepath.Join(other, ".git")+"\n"), 0o644)
	if g, err := ReadCheckout(ctx, f, "forged"); err != nil || g.Withheld == "" || g.Revision != "" {
		t.Fatalf("a gitfile naming another repository: %+v, %v", g, err)
	}

	// A gitfile naming this repository's worktree directory, from a
	// directory the worktree's back-link does not name.
	os.MkdirAll(filepath.Join(dir, "copy"), 0o755)
	data, _ := os.ReadFile(filepath.Join(dir, "wt", ".git"))
	os.WriteFile(filepath.Join(dir, "copy", ".git"), data, 0o644)
	if g, err := ReadCheckout(ctx, f, "copy"); err != nil || g.Withheld == "" || g.Revision != "" {
		t.Fatalf("a copied gitfile: %+v, %v", g, err)
	}

	// The repository's config names a filter: the worktree is withheld
	// too, and the filter never runs.
	mark := filepath.Join(t.TempDir(), "ran")
	gitRun(t, dir, "config", "filter.x.clean", "touch "+mark+"; cat")
	os.WriteFile(filepath.Join(dir, "wt", ".gitattributes"), []byte("*.txt filter=x\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "wt", "a.txt"), []byte("b\n"), 0o644)
	if g, err := ReadCheckout(ctx, f, "wt"); err != nil || g.Withheld == "" || g.Revision != "" {
		t.Fatalf("a worktree of a repository with a filter: %+v, %v", g, err)
	}
	if _, err := GitDiffIn(ctx, f, "wt", "", 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mark); err == nil {
		t.Fatal("the repository's filter ran")
	}
}
