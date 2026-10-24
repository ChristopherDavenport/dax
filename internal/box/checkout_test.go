package box

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeURL(t *testing.T) {
	for _, tc := range []struct {
		in, want, err string
	}{
		{in: "https://github.com/me/app", want: "https://github.com/me/app"},
		{in: "https://GitHub.com/me/App.git", want: "https://github.com/me/App"},
		{in: "https://github.com/me/app/", want: "https://github.com/me/app"},
		{in: "  https://github.com/me/app.git/  ", want: "https://github.com/me/app"},
		{in: "git@github.com:me/app.git", want: "ssh://git@github.com/me/app"},
		{in: "git@github.com:/me/app", want: "ssh://git@github.com/me/app"},
		{in: "ssh://git@Example.org:2222/srv/app.git", want: "ssh://git@example.org:2222/srv/app"},
		{in: "file:///srv/git/app.git", want: "file:///srv/git/app"},
		{in: "file://localhost/srv/git/app", want: "file:///srv/git/app"},
		{in: "https://tok@github.com/me/app", err: "a user in an https URL is a credential"},
		{in: "https://x-access-token:ghp_abc@github.com/me/app", err: "a password"},
		{in: "ssh://me:pw@host/app", err: "a password"},
		{in: "http://github.com/me/app", err: "want https://"},
		{in: "ftp://h/app", err: "want https://"},
		{in: "/srv/git/app", err: "want https://"},
		{in: "app", err: "want https://"},
		{in: "file://host/srv/app", err: "no host"},
		{in: "https://github.com/me/../app", err: `a path with ".."`},
		{in: "https://github.com/", err: "no repository path"},
		{in: "https://github.com/me/app?x=1", err: "no query"},
		{in: "-uhttps://x/y", err: "not a repository URL"},
		{in: "https://github.com/me/a b", err: "not a repository URL"},
		{in: "", err: "an empty URL"},
	} {
		got, err := NormalizeURL(tc.in)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("NormalizeURL(%q) = %q, %v; want an error naming %q", tc.in, got, err, tc.err)
			}
			if err != nil && strings.Contains(err.Error(), "ghp_abc") {
				t.Errorf("NormalizeURL(%q)'s error holds the token: %v", tc.in, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("NormalizeURL(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		"https://tok@github.com/me/app": "https://github.com/me/app",
		"https://u:p@github.com/me/app": "https://github.com/me/app",
		"ssh://git@github.com/me/app":   "ssh://github.com/me/app",
		"https://github.com/me/app":     "https://github.com/me/app",
		"file:///srv/app":               "file:///srv/app",
		"https://a@b@host":              "https://host",
		"not a url":                     "not a url",
	} {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckoutsAllows(t *testing.T) {
	c := Checkouts{Allow: []string{"https://GitHub.com/me/*", "git@gitlab.com:team/*.git", "file:///srv/git/*/"}}
	if err := c.check(); err != nil {
		t.Fatal(err)
	}
	for u, want := range map[string]bool{
		"https://github.com/me/app":      true,
		"https://github.com/me/app/sub":  false, // * does not cross /
		"https://github.com/other/app":   false,
		"ssh://git@gitlab.com/team/app":  true,
		"ssh://git@gitlab.com/other/app": false,
		"file:///srv/git/app":            true,
		"https://evil.com/me/app":        false,
	} {
		if got := c.Allows(u); got != want {
			t.Errorf("Allows(%q) = %v, want %v", u, got, want)
		}
	}
	if (Checkouts{}).Allows("https://github.com/me/app") {
		t.Error("no allow took a URL")
	}
	for _, bad := range []Checkouts{{Allow: []string{""}}, {Allow: []string{"https://[x"}}, {Dir: "rel/dir"}} {
		if err := bad.check(); err == nil {
			t.Errorf("check(%+v) took it", bad)
		}
	}
}

func TestCheckoutPath(t *testing.T) {
	c := Checkouts{Dir: "/srv/checkouts"}
	a, err := c.Path("https://github.com/me/App")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := c.Path("https://gitlab.com/me/App")
	if filepath.Dir(a) != "/srv/checkouts" || !strings.HasPrefix(filepath.Base(a), "app-") || len(filepath.Base(a)) != len("app-")+12 {
		t.Errorf("Path = %q", a)
	}
	if a == b {
		t.Errorf("two repositories of one name share %q", a)
	}
	if again, _ := c.Path("https://github.com/me/App"); again != a {
		t.Errorf("Path is %q then %q", a, again)
	}
	for in, want := range map[string]string{
		"https://github.com/me/My.Repo": "my.repo",
		"ssh://git@h/srv/app":           "app",
		"file:///x/---":                 "repo",
	} {
		if got := RepoName(in); got != want {
			t.Errorf("RepoName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckRef(t *testing.T) {
	for _, ok := range []string{"main", "release/1.2", "v0.0.1", "feature_x-y"} {
		if err := CheckRef("ref", ok); err != nil {
			t.Errorf("CheckRef(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-x", "/main", "a..b", "a//b", "main/", "x.lock", "a b", "main.", "a;b"} {
		if err := CheckRef("ref", bad); err == nil {
			t.Errorf("CheckRef(%q) took it", bad)
		}
	}
}

// gitEnv isolates git from the machine's config and gives commits a
// name, for the tests that run it.
func gitEnv(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	empty := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "test")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "test@example.invalid")
	}
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %q in %s: %v", args, dir, err)
	}
	return out
}

// upstream is a repository with one commit on main, and its file URL.
func upstream(t *testing.T) (dir, url string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "up")
	gitRun(t, filepath.Dir(dir), "init", "--quiet", "-b", "main", dir)
	commit(t, dir, "a.txt", "one")
	return dir, "file://" + dir
}

func commit(t *testing.T, dir, file, text string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", file)
	gitRun(t, dir, "commit", "--quiet", "-m", file+" "+text)
	return gitRun(t, dir, "rev-parse", "HEAD")
}

func TestSyncCheckout(t *testing.T) {
	gitEnv(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		ref   string
		after func(t *testing.T, up, co string) // what happens between the clone and the next start
		note  string
		moved bool // the checkout's head is the upstream's after
	}{
		{name: "up to date", note: "up to date with origin/main", moved: true},
		{
			name:  "upstream moved on, worktree clean: fast-forwarded",
			after: func(t *testing.T, up, _ string) { commit(t, up, "b.txt", "two") },
			note:  "fast-forwarded to origin/main", moved: true,
		},
		{
			name: "uncommitted changes: left",
			after: func(t *testing.T, up, co string) {
				commit(t, up, "b.txt", "two")
				os.WriteFile(filepath.Join(co, "a.txt"), []byte("mine"), 0o644)
			},
			note: "uncommitted changes; left as it is",
		},
		{
			name:  "a session's commit, upstream unchanged: left",
			after: func(t *testing.T, _, co string) { commit(t, co, "c.txt", "mine") },
			note:  "commits origin/main does not; left as it is",
		},
		{
			name: "diverged: left",
			after: func(t *testing.T, up, co string) {
				commit(t, up, "b.txt", "two")
				commit(t, co, "c.txt", "mine")
			},
			note: "diverged from origin/main; left as it is",
		},
		{
			name: "another branch checked out: left",
			after: func(t *testing.T, up, co string) {
				commit(t, up, "b.txt", "two")
				gitRun(t, co, "checkout", "--quiet", "-b", "work")
			},
			note: "on branch work, not main; left as it is",
		},
		{
			name: "a branch the session names",
			ref:  "dev",
			after: func(t *testing.T, up, _ string) {
				gitRun(t, up, "checkout", "--quiet", "dev")
				commit(t, up, "d.txt", "dev two")
				gitRun(t, up, "checkout", "--quiet", "main")
			},
			note: "fast-forwarded to origin/dev",
		},
		{
			name:  "a tag",
			ref:   "v1",
			note:  "up to date with refs/tags/v1",
			moved: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, url := upstream(t)
			if tc.ref == "dev" {
				gitRun(t, up, "branch", "dev")
			}
			if tc.ref == "v1" {
				gitRun(t, up, "tag", "v1")
			}
			co := filepath.Join(t.TempDir(), "checkouts", "up-0123456789ab")
			st, err := SyncCheckout(ctx, co, url, tc.ref)
			if err != nil {
				t.Fatal(err)
			}
			if st.Note != "cloned" || st.Head != gitRun(t, up, "rev-parse", "HEAD") {
				t.Fatalf("first start: %+v", st)
			}
			if tc.after != nil {
				tc.after(t, up, co)
			}
			before := gitRun(t, co, "rev-parse", "HEAD")
			st, err = SyncCheckout(ctx, co, url, tc.ref)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(st.Note, tc.note) || !strings.HasPrefix(st.Note, "fetched; ") {
				t.Errorf("note = %q, want %q in it", st.Note, tc.note)
			}
			upHead := gitRun(t, up, "rev-parse", "HEAD")
			if tc.moved && st.Head != upHead {
				t.Errorf("head %s, want the upstream's %s", st.Head, upHead)
			}
			if strings.HasSuffix(tc.note, "left as it is") && st.Head != before {
				t.Errorf("a checkout left as it is moved: %s to %s", before, st.Head)
			}
			if tc.name == "uncommitted changes: left" {
				if b, _ := os.ReadFile(filepath.Join(co, "a.txt")); string(b) != "mine" || !st.Dirty {
					t.Errorf("the session's change is %q, dirty %v", b, st.Dirty)
				}
			}
		})
	}
}

func TestSyncCheckoutRefusals(t *testing.T) {
	gitEnv(t)
	ctx := context.Background()
	up, url := upstream(t)
	other, otherURL := upstream(t)
	_ = other
	co := filepath.Join(t.TempDir(), "co")
	if _, err := SyncCheckout(ctx, co, url, ""); err != nil {
		t.Fatal(err)
	}
	commit(t, up, "b.txt", "two")
	// The directory holds another repository: touched nowhere.
	before := gitRun(t, co, "rev-parse", "HEAD")
	if _, err := SyncCheckout(ctx, co, otherURL, ""); err == nil || !strings.Contains(err.Error(), "another repository") {
		t.Errorf("err = %v", err)
	}
	if after := gitRun(t, co, "rev-parse", "HEAD"); after != before {
		t.Errorf("moved from %s to %s", before, after)
	}
	// A ref the remote does not have.
	st, err := SyncCheckout(ctx, co, url, "nope")
	if err != nil || !strings.Contains(st.Note, "no branch or tag nope") {
		t.Errorf("%+v %v", st, err)
	}
	// A clone of a ref that is not there fails, and leaves nothing.
	co2 := filepath.Join(t.TempDir(), "co2")
	if _, err := SyncCheckout(ctx, co2, url, "nope"); err == nil {
		t.Error("cloned a ref that is not there")
	}
	// A repository that is not there.
	if _, err := SyncCheckout(ctx, filepath.Join(t.TempDir(), "co3"), "file:///nonexistent/repo", ""); err == nil {
		t.Error("cloned nothing")
	}
}

func TestBundleRoundTrip(t *testing.T) {
	gitEnv(t)
	ctx := context.Background()
	src, _ := upstream(t)
	head := commit(t, src, "b.txt", "two")
	b, err := BundleHead(ctx, src, MaxBundle)
	if err != nil {
		t.Fatal(err)
	}
	if b.Head != head || b.Branch != "main" || b.Dirty || b.Size == 0 {
		t.Fatalf("bundle %+v", b)
	}
	dst := filepath.Join(t.TempDir(), "dst")
	gitRun(t, filepath.Dir(dst), "init", "--quiet", "-b", "trunk", dst)
	before, err := FetchBundle(ctx, dst, b, "dax-box/up", false)
	if err != nil || before != "" {
		t.Fatalf("first fetch: %q %v", before, err)
	}
	if got := gitRun(t, dst, "rev-parse", "dax-box/up"); got != head {
		t.Errorf("branch at %s, want %s", got, head)
	}
	if out := gitRun(t, dst, "for-each-ref", "refs/dax-box"); out != "" {
		t.Errorf("the incoming ref was left: %q", out)
	}
	// Again: already there.
	if before, err := FetchBundle(ctx, dst, b, "dax-box/up", false); err != nil || before != head {
		t.Errorf("again: %q %v", before, err)
	}
	// Forward: the box moved on.
	head2 := commit(t, src, "c.txt", "three")
	b2, _ := BundleHead(ctx, src, MaxBundle)
	if before, err := FetchBundle(ctx, dst, b2, "dax-box/up", false); err != nil || before != head {
		t.Errorf("forward: %q %v", before, err)
	}
	if got := gitRun(t, dst, "rev-parse", "dax-box/up"); got != head2 {
		t.Errorf("branch at %s, want %s", got, head2)
	}
	// Backward or sideways only with force.
	if _, err := FetchBundle(ctx, dst, b, "dax-box/up", false); err == nil || !strings.Contains(err.Error(), "give -force") {
		t.Errorf("backward: %v", err)
	}
	if _, err := FetchBundle(ctx, dst, b, "dax-box/up", true); err != nil {
		t.Errorf("backward with force: %v", err)
	}
	if got := gitRun(t, dst, "rev-parse", "dax-box/up"); got != head {
		t.Errorf("forced branch at %s, want %s", got, head)
	}
	// The branch checked out here is refused.
	gitRun(t, dst, "checkout", "--quiet", "dax-box/up")
	if _, err := FetchBundle(ctx, dst, b2, "dax-box/up", true); err == nil || !strings.Contains(err.Error(), "checked out here") {
		t.Errorf("checked out: %v", err)
	}
	// A bundle whose head is not the one named.
	lie := b2
	lie.Head = head
	if _, err := FetchBundle(ctx, dst, lie, "other", false); err == nil || !strings.Contains(err.Error(), "not the head") {
		t.Errorf("lie: %v", err)
	}
	// Not a bundle.
	junk := b2
	junk.Data = []byte("not a bundle")
	if _, err := FetchBundle(ctx, dst, junk, "other", false); err == nil || !strings.Contains(err.Error(), "does not verify") {
		t.Errorf("junk: %v", err)
	}
	// A branch name git refuses.
	if _, err := FetchBundle(ctx, dst, b2, "a..b", false); err == nil {
		t.Error("took branch a..b")
	}
	// Too big, and uncommitted changes said.
	if _, err := BundleHead(ctx, src, 1); err == nil || !strings.Contains(err.Error(), "more than the 1") {
		t.Errorf("max: %v", err)
	}
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("dirty"), 0o644)
	if b3, err := BundleHead(ctx, src, MaxBundle); err != nil || !b3.Dirty {
		t.Errorf("dirty: %+v %v", b3, err)
	}
	// Not a repository.
	if _, err := BundleHead(ctx, t.TempDir(), MaxBundle); err == nil || !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("no repo: %v", err)
	}
}
