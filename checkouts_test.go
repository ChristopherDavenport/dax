package dax

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agenteval"
	"github.com/ChristopherDavenport/agentsession"
	workspace "github.com/ChristopherDavenport/agentworkspace"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/ext/coding"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/policy"
)

// envsOn are the env entries on the record's path, in order.
func envsOn(rec *agentsession.Session) []*agentsession.EnvEntry {
	var out []*agentsession.EnvEntry
	for _, e := range rec.Path(rec.Leaf()) {
		if v, ok := e.(*agentsession.EnvEntry); ok {
			out = append(out, v)
		}
	}
	return out
}

// movesOf folds agentsession.CheckoutChanges over the env entries that
// list checkouts and returns the kinds of change to path, in order.
func movesOf(envs []*agentsession.EnvEntry, path string) []agentsession.CheckoutChangeKind {
	var out []agentsession.CheckoutChangeKind
	var before *agentsession.EnvEntry
	for _, e := range envs {
		if e.Checkouts == nil {
			continue
		}
		for _, c := range agentsession.CheckoutChanges(before, e) {
			if c.Path == path {
				out = append(out, c.Kind)
			}
		}
		before = e
	}
	return out
}

// subsequence is whether want appears in got in order.
func subsequence[T comparable](got, want []T) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}

// checkoutRepo is snapshotRepo with a bare remote, origin, that main is
// pushed to and tracks; it returns the commit.
func checkoutRepo(t *testing.T, dir string) string {
	t.Helper()
	base := snapshotRepo(t, dir)
	remote := filepath.Join(t.TempDir(), "remote.git")
	snapGit(t, filepath.Dir(remote), "init", "-q", "--bare", remote)
	snapGit(t, dir, "branch", "-M", "main")
	snapGit(t, dir, "remote", "add", "origin", remote)
	snapGit(t, dir, "push", "-q", "-u", "origin", "main")
	return base
}

// A run that adds a worktree, commits in it and pushes it records each
// as a move of the worktree's checkout, in its env entries: made at the
// commit it branched from, then a new revision, then the upstream at
// that revision; the checkout at the root carries the entry's own vcs;
// another worktree of the repository the session never worked in is
// not listed; and the run's end snapshots the worktree it changed by
// its path, which agenteval reads back.
func TestCheckoutsFollowAWorktree(t *testing.T) {
	dir := t.TempDir()
	base := checkoutRepo(t, dir)
	snapGit(t, dir, "worktree", "add", "-q", "-b", "idle", "idle")
	m := &steps{parent: [][2]string{
		{"bash", `{"command":"git worktree add -q -b fix wt"}`},
		{"write", `{"path":"wt/new.txt","content":"new\n"}`},
		{"bash", `{"command":"git -C wt add new.txt && git -C wt -c user.name=n -c user.email=e@x commit -qm two"}`},
		{"bash", `{"command":"git -C wt push -q -u origin fix"}`},
	}}
	sess := snapshotSession(t, dir, m)
	got, stderr := runSnapshot(t, sess, true, 0)
	if stderr != "" {
		t.Errorf("dax said %q", stderr)
	}
	head := snapGit(t, filepath.Join(dir, "wt"), "rev-parse", "HEAD")
	rec := recordOf(t, sess)
	envs := envsOn(rec)
	if kinds := movesOf(envs, "wt"); !subsequence(kinds, []agentsession.CheckoutChangeKind{agentsession.CheckoutAdded, agentsession.CheckoutRevision, agentsession.CheckoutUpstream}) {
		t.Errorf("the worktree's moves %v, want added, then revision, then upstream", kinds)
	}
	var first, last *agentsession.Checkout
	for _, e := range envs {
		if e.Checkout("idle") != nil {
			t.Errorf("an env entry lists the worktree the session never worked in: %+v", e.Checkouts)
		}
		if co := e.Checkout("."); co != nil && (co.VCS == nil || e.VCS == nil || co.VCS.Revision != e.VCS.Revision || co.VCS.Dirty != e.VCS.Dirty) {
			t.Errorf("the checkout at . %+v and the entry's vcs %+v differ", co.VCS, e.VCS)
		}
		if co := e.Checkout("wt"); co != nil {
			if first == nil {
				first = co
			}
			last = co
		}
	}
	if first == nil || first.VCS == nil || first.VCS.Revision != base || first.Branch != "fix" || first.Repo != ".git" {
		t.Fatalf("the worktree first listed as %+v, want branch fix at %s in .git", first, base)
	}
	if last.VCS.Revision != head || last.Upstream == nil || last.Upstream.Name != "origin/fix" || last.Upstream.Revision != head {
		t.Errorf("the worktree last listed as %+v %+v, want %s pushed to origin/fix", last.VCS, last.Upstream, head)
	}
	for _, e := range envs {
		for _, c := range e.Checkouts {
			if c.Upstream != nil && strings.Contains(c.Upstream.Name, "remote.git") {
				t.Errorf("a checkout names the remote's URL: %+v", c.Upstream)
			}
		}
	}

	// The worktree's snapshot, against the commit it was made at.
	if len(got.CheckoutSnapshots) != 1 || got.CheckoutSnapshots[0].Path != "wt" || got.CheckoutSnapshots[0].Base != base || got.CheckoutSnapshots[0].Error != "" {
		t.Fatalf("-result's checkout snapshots %+v, want the worktree's against %s", got.CheckoutSnapshots, base)
	}
	snaps, _ := snapshotsOn(rec)
	if r := snaps[got.CheckoutSnapshots[0].Entry]; r.Path != "wt" || !strings.Contains(string(r.Data), "new.txt") {
		t.Errorf("the worktree's snapshot entry %+v, want its patch, new.txt in it", r)
	}
	res, err := agenteval.ResultAt(t.Context(), storeOf(t, sess), sess.ID(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.CheckoutSnapshots) != 1 || res.CheckoutSnapshots[0].Path != "wt" {
		t.Errorf("agenteval reads the checkout snapshots %+v, want the worktree's", res.CheckoutSnapshots)
	}
}

// A commit no call said was git's, a script's, is read at the run's
// end; and a git command in one checkout reads that one alone, so the
// root's commit the script moved is not read until then.
func TestCheckoutsReadAtTheRunsEnd(t *testing.T) {
	dir := t.TempDir()
	snapshotRepo(t, dir)
	snapGit(t, dir, "worktree", "add", "-q", "-b", "fix", "wt")
	m := &steps{parent: [][2]string{
		{"bash", `{"command":"git -C wt status"}`},
		{"write", `{"path":"commit.sh","content":"git -c user.name=n -c user.email=e@x commit -qam scripted\n"}`},
		{"write", `{"path":"mod","content":"changed\n"}`},
		{"bash", `{"command":"sh commit.sh"}`},
		{"bash", `{"command":"git -C wt -c user.name=n -c user.email=e@x commit -q --allow-empty -m mark"}`},
	}}
	sess := snapshotSession(t, dir, m)
	before := snapGit(t, dir, "rev-parse", "HEAD")
	runSnapshot(t, sess, false, 0)
	after := snapGit(t, dir, "rev-parse", "HEAD")
	wt := snapGit(t, filepath.Join(dir, "wt"), "rev-parse", "HEAD")
	if before == after {
		t.Fatal("the script made no commit")
	}
	envs := envsOn(recordOf(t, sess))
	// The entry the worktree's commit wrote: the worktree moved, the
	// root's commit as the run's start read it.
	var mid *agentsession.EnvEntry
	for _, e := range envs {
		if co := e.Checkout("wt"); co != nil && co.VCS != nil && co.VCS.Revision == wt && mid == nil {
			mid = e
		}
	}
	if mid == nil {
		t.Fatalf("no env entry lists the worktree at %s: %+v", wt, envs)
	}
	if root := mid.Checkout("."); root == nil || root.VCS == nil || root.VCS.Revision != before {
		t.Errorf("the worktree's commit read the root too: %+v, want it at %s", root, before)
	}
	last := envs[len(envs)-1]
	if root := last.Checkout("."); root == nil || root.VCS == nil || root.VCS.Revision != after {
		t.Errorf("after the run the root is listed as %+v, want the script's commit %s", root, after)
	}
}

// A workspace with no git records no checkouts, not an empty list, and
// says so once; a run's snapshot is not begun, and the error saying git
// is missing comes once.
func TestCheckoutsWithoutGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	snapshotRepo(t, dir)
	ws, err := workspace.NewLocal(dir, []string{"PATH=" + t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	var mu sync.Mutex
	var said []string
	base := t.TempDir()
	sess, err := agent.New(t.Context(), agent.Options{
		Model: "scripted", Streamer: &steps{}, Dir: dir, Root: filepath.Join(base, "sessions"),
		UserDir: filepath.Join(base, "user"), Workspace: ws,
		Extensions: []extension.Extension{coding.New(0)},
		Policy:     &policy.Settings{Fallback: "allow"},
		Log: func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			said = append(said, fmt.Sprintf(format, args...))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	_, stderr := runSnapshot(t, sess, true, 0)
	if !strings.Contains(stderr, "git is not installed") {
		t.Errorf("the first run said %q, want git missing", stderr)
	}
	if err := sess.StartSnapshot(t.Context(), 0); err != nil {
		t.Errorf("a later run's snapshot: %v, want nothing said", err)
	}
	for _, e := range envsOn(recordOf(t, sess)) {
		if e.Checkouts != nil {
			t.Errorf("an env entry lists checkouts %+v with no git", e.Checkouts)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	n := 0
	for _, s := range said {
		if strings.Contains(s, "git is not installed") {
			n++
		}
	}
	if n > 1 {
		t.Errorf("git's absence was said %d times: %q", n, said)
	}
}

// A colocated jj repository is listed where it is, with no vcs, since
// git's reading of a jj working copy is churn.
func TestCheckoutsColocatedJJ(t *testing.T) {
	dir := t.TempDir()
	snapshotRepo(t, dir)
	if err := os.Mkdir(filepath.Join(dir, ".jj"), 0o755); err != nil {
		t.Fatal(err)
	}
	sess := snapshotSession(t, dir, &steps{})
	runSnapshot(t, sess, false, 0)
	envs := envsOn(recordOf(t, sess))
	if len(envs) == 0 {
		t.Fatal("no env entry")
	}
	for _, e := range envs {
		co := e.Checkout(".")
		if co == nil || co.VCS != nil || co.Branch != "" || e.VCS != nil {
			t.Errorf("a jj checkout listed as %+v, the entry's vcs %+v, want where it is alone", co, e.VCS)
		}
	}
	if !slices.ContainsFunc(envs, func(e *agentsession.EnvEntry) bool { return e.Checkout(".") != nil }) {
		t.Error("the jj checkout is not listed")
	}
}

// A resumed session goes on following the checkouts its record
// followed: a worktree the first process worked in is read again at
// the next run's start, its move since on the record, though no call
// of the new run names it.
func TestCheckoutsCarryOverAResume(t *testing.T) {
	dir := t.TempDir()
	snapshotRepo(t, dir)
	base := t.TempDir()
	o := agent.Options{
		Model: "scripted", Dir: dir, Root: filepath.Join(base, "sessions"),
		UserDir:    filepath.Join(base, "user"),
		Extensions: []extension.Extension{coding.New(0)},
		Policy:     &policy.Settings{Fallback: "allow"},
	}
	o.Streamer = &steps{parent: [][2]string{{"bash", `{"command":"git worktree add -q -b fix wt"}`}}}
	first, err := agent.New(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	runSnapshot(t, first, false, 0)
	id := first.ID()
	first.Close()

	snapGit(t, filepath.Join(dir, "wt"), "-c", "user.name=n", "-c", "user.email=e@x", "commit", "-q", "--allow-empty", "-m", "between")
	moved := snapGit(t, filepath.Join(dir, "wt"), "rev-parse", "HEAD")
	o.Streamer = &steps{}
	second, err := agent.Resume(t.Context(), o, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close() })
	runSnapshot(t, second, false, 0)
	envs := envsOn(recordOf(t, second))
	last := envs[len(envs)-1]
	if co := last.Checkout("wt"); co == nil || co.VCS == nil || co.VCS.Revision != moved {
		t.Errorf("after the resume the worktree is listed as %+v, want it at %s", co, moved)
	}
}
