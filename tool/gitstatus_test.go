package tool

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir, "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// The status line's git segment: the branch, dirty or not, read
// through the workspace; a repository whose own config would run a
// program is not read at all.
func TestGitHeadReadsTheBranchAndWhetherTheTreeIsDirty(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644)
	gitRun(t, dir, "add", "a.txt")
	gitRun(t, dir, "commit", "-q", "-m", "one")
	f := newWS(t, dir)
	ctx := context.Background()

	g, err := GitHead(ctx, f)
	if err != nil || g != (Git{Repo: true, Branch: "main"}) {
		t.Fatalf("clean: %+v, %v", g, err)
	}
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("b\n"), 0o644)
	if g, err = GitHead(ctx, f); err != nil || g != (Git{Repo: true, Branch: "main", Dirty: true}) {
		t.Fatalf("dirty: %+v, %v", g, err)
	}

	// Not a repository: nothing to show.
	if g, err := GitHead(ctx, newWS(t, t.TempDir())); err != nil || g.Repo {
		t.Fatalf("no repository: %+v, %v", g, err)
	}

	// A clean filter the repository sets is a program git status would
	// run: the head is not read, and the filter never runs.
	mark := filepath.Join(t.TempDir(), "ran")
	gitRun(t, dir, "config", "filter.x.clean", "touch "+mark+"; cat")
	os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("*.txt filter=x\n"), 0o644)
	g, err = GitHead(ctx, f)
	if err != nil || !g.Repo || g.Branch != "" || g.Withheld == "" {
		t.Fatalf("a filter: %+v, %v", g, err)
	}
	if _, err := os.Stat(mark); err == nil {
		t.Fatal("the repository's filter ran")
	}
}
