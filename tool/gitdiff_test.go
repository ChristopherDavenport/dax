package tool

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	workspace "github.com/ChristopherDavenport/agentworkspace"
)

// GitDiff reads the same patch through every workspace the tools may
// act in: this directory, a container, one served over remote and one
// through a tunnel. The patch holds a change, a deletion and an
// untracked file, and not an ignored one; reading it leaves the
// repository's index as it was (#191).
func TestGitDiffIsTheSameInEveryWorkspace(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "main")
	for name, body := range map[string]string{"keep": "a\n", "gone": "b\n", "mod": "c\n", ".gitignore": "ignored\n"} {
		os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "one")
	os.Remove(filepath.Join(dir, "gone"))
	os.WriteFile(filepath.Join(dir, "mod"), []byte("c\nc2\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "untracked"), []byte("u\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "ignored"), []byte("secret\n"), 0o644)
	index, err := os.ReadFile(filepath.Join(dir, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	local, err := workspace.NewLocal(dir, DefaultEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { local.Close() })
	ctx := context.Background()
	var first GitChange
	for _, tc := range []struct {
		name string
		ws   workspace.Workspace
	}{
		{"local", local}, {"container", newMapped(t, dir)}, {"remote", served(t, newMapped(t, dir))}, {"tunnel", tunneled(t, newMapped(t, dir))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := GitDiff(ctx, NewFiles(tc.ws), "", 1<<20)
			if err != nil || !c.Repo || !c.Commit || c.Withheld != "" || c.Size != int64(len(c.Data)) || c.Size == 0 {
				t.Fatalf("%+v, %v", c, err)
			}
			patch := string(c.Data)
			for _, want := range []string{"deleted file mode", "+c2", "b/untracked", "+u"} {
				if !strings.Contains(patch, want) {
					t.Errorf("the patch lacks %q:\n%s", want, patch)
				}
			}
			if strings.Contains(patch, "secret") {
				t.Errorf("the patch holds the ignored file:\n%s", patch)
			}
			if first.Repo && (c.Base != first.Base || !bytes.Equal(c.Data, first.Data)) {
				t.Errorf("against %s:\n%s\nwant the local workspace's, against %s:\n%s", c.Base, c.Data, first.Base, first.Data)
			}
			if !first.Repo {
				first = c
			}
			// Against the commit named, and over a bound: the size alone.
			again, err := GitDiff(ctx, NewFiles(tc.ws), c.Base, 16)
			if err != nil || again.Base != c.Base || again.Size != c.Size || again.Data != nil {
				t.Errorf("over the bound: %+v, %v, want its size and no data", again, err)
			}
			if after, _ := os.ReadFile(filepath.Join(dir, ".git", "index")); !bytes.Equal(after, index) {
				t.Error("GitDiff changed the repository's index")
			}
		})
	}
}

// Outside a work tree GitDiff is nothing and no error; a repository
// whose own config names a filter is not read, and the filter never
// runs; a repository with no commit is read against the empty tree; a
// base that is not a commit is an error.
func TestGitDiffWithoutAPatch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	ctx := context.Background()
	if c, err := GitDiff(ctx, newWS(t, t.TempDir()), "", 1<<20); err != nil || c.Repo {
		t.Errorf("outside git: %+v, %v", c, err)
	}

	empty := t.TempDir()
	gitRun(t, empty, "init", "-q")
	os.WriteFile(filepath.Join(empty, "f"), []byte("x\n"), 0o644)
	c, err := GitDiff(ctx, newWS(t, empty), "", 1<<20)
	if err != nil || !c.Repo || c.Commit || c.Base == "" || !strings.Contains(string(c.Data), "b/f") {
		t.Errorf("no commit: %+v, %v, want the file against the empty tree", c, err)
	}
	if _, err := GitDiff(ctx, newWS(t, empty), "0123456789abcdef0123456789abcdef01234567", 1<<20); err == nil {
		t.Error("a base that is no commit: no error")
	}

	dir := t.TempDir()
	gitRun(t, dir, "init", "-q")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644)
	gitRun(t, dir, "add", "a.txt")
	gitRun(t, dir, "commit", "-q", "-m", "one")
	mark := filepath.Join(t.TempDir(), "ran")
	gitRun(t, dir, "config", "filter.x.clean", "touch "+mark+"; cat")
	os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("*.txt filter=x\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("b\n"), 0o644)
	c, err = GitDiff(ctx, newWS(t, dir), "", 1<<20)
	if err != nil || !c.Repo || c.Withheld == "" || c.Data != nil {
		t.Errorf("a filter: %+v, %v, want it withheld", c, err)
	}
	if _, err := os.Stat(mark); err == nil {
		t.Error("the repository's filter ran")
	}
}
