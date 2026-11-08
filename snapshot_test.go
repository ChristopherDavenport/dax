package dax

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentconsole/client"
	"github.com/ChristopherDavenport/agenteval"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/ext/coding"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/policy"
)

// snapGit runs git in dir with the user's own config left out, and
// returns its standard output.
func snapGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// snapshotRepo is a git repository with one commit of keep, gone, mod
// and a .gitignore that ignores ignored; it returns the commit.
func snapshotRepo(t *testing.T, dir string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	snapGit(t, dir, "init", "-q")
	for name, body := range map[string]string{"keep": "a\n", "gone": "b\n", "mod": "c\n", ".gitignore": "ignored\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	snapGit(t, dir, "add", ".")
	snapGit(t, dir, "-c", "user.name=n", "-c", "user.email=e@x", "commit", "-qm", "base")
	return snapGit(t, dir, "rev-parse", "HEAD")
}

// snapshotSession is a recorded session for -p in dir over streamer,
// whose every call runs unasked.
func snapshotSession(t *testing.T, dir string, streamer openresponses.Streamer) *agent.Session {
	t.Helper()
	base := t.TempDir()
	o := agent.Options{
		Model: "scripted", Streamer: streamer, Dir: dir, Root: filepath.Join(base, "sessions"),
		UserDir:    filepath.Join(base, "user"),
		Extensions: []extension.Extension{coding.New(0)},
		Policy:     &policy.Settings{Fallback: "allow"},
	}
	sess, err := agent.New(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

// runSnapshot runs prompt under -p with -result and the snapshot set
// as given, and returns the result and what dax said on standard error.
func runSnapshot(t *testing.T, sess *agent.Session, on bool, max int64) (runResult, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "result.json")
	var stderr bytes.Buffer
	f := &printFront{info: frontInfo{Name: "dax", Prompt: "go", Result: path, Snapshot: on, SnapshotMax: max}, in: bufio.NewScanner(strings.NewReader("")), stderr: &stderr}
	if err := f.run(t.Context(), sess, sess); err != nil {
		t.Fatalf("run: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got runResult
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("%s: %v", data, err)
	}
	return got, strings.ReplaceAll(stderr.String(), "session: "+sess.ID()+"\n", "")
}

// recordOf is the session's record as the store holds it.
func recordOf(t *testing.T, sess *agent.Session) *agentsession.Session {
	t.Helper()
	r, err := client.RecordOf(sess.Recorder())
	if err != nil {
		t.Fatal(err)
	}
	rec, err := r.Read(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// snapshotsOn are the snapshot records on the record's path, by entry
// ID, and the IDs in order.
func snapshotsOn(rec *agentsession.Session) (map[string]agenteval.SnapshotRecord, []string) {
	got := map[string]agenteval.SnapshotRecord{}
	var ids []string
	for _, e := range rec.Path(rec.Leaf()) {
		if c, ok := e.(*agentsession.CustomEntry); ok {
			if r, ok := agenteval.ReadSnapshot(c); ok {
				got[c.ID] = r
				ids = append(ids, c.ID)
			}
		}
	}
	return got, ids
}

// treeOf is every file under dir but .git and ignored, by its path, as
// bytes.
func treeOf(t *testing.T, dir string, skip ...string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() {
			if rel == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		for _, s := range skip {
			if rel == s {
				return nil
			}
		}
		data, err := os.ReadFile(p)
		files[rel] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// A -p run in a git tree records the change it left as agenteval's
// snapshot entry, after the run's last entry: its patch applied to the
// commit the run started from gives the tree, a new untracked file and
// a deleted one among it, an ignored file not; -result names the entry,
// and the run's env entry names the commit (#191).
func TestPrintSnapshotReproducesTheTree(t *testing.T) {
	dir := t.TempDir()
	base := snapshotRepo(t, dir)
	m := &steps{parent: [][2]string{
		{"write", `{"path":"sub/new.txt","content":"new\n"}`},
		{"bash", `{"command":"rm gone && printf 'c2\\n' >> mod && printf 'secret\\n' > ignored && printf 'u\\n' > untracked"}`},
	}}
	sess := snapshotSession(t, dir, m)
	got, stderr := runSnapshot(t, sess, true, 0)
	if stderr != "" {
		t.Errorf("dax said %q", stderr)
	}
	if got.StartSnapshot != nil {
		t.Errorf("a clean start wrote a start snapshot: %+v", got.StartSnapshot)
	}
	s := got.Snapshot
	if s == nil || s.Entry == "" || s.Base != base || s.Error != "" || !strings.HasPrefix(s.Digest, "sha256:") || s.Size == 0 {
		t.Fatalf("-result's snapshot %+v, want the entry with base %s and a digest", s, base)
	}
	rec := recordOf(t, sess)
	snaps, ids := snapshotsOn(rec)
	r, ok := snaps[s.Entry]
	if !ok || len(ids) != 1 {
		t.Fatalf("the record holds snapshots %v, want the one -result names, %s", ids, s.Entry)
	}
	if r.Digest != s.Digest || agentsession.HashBytes(r.Data) != r.Digest || r.Base != base {
		t.Errorf("the entry's digest %s, base %s, want -result's %s and %s", r.Digest, r.Base, s.Digest, base)
	}
	// The target is the run's last entry, the one before the snapshot.
	path := rec.Path(rec.Leaf())
	if len(path) < 2 || path[len(path)-1].Base().ID != s.Entry || path[len(path)-2].Base().ID != r.Target {
		t.Errorf("the snapshot targets %s, want the entry before it", r.Target)
	}
	if end, ok := path[len(path)-2].(*agentsession.RunEntry); !ok || !end.IsEnd() {
		t.Errorf("the entry before the snapshot is %T, want the run's end", path[len(path)-2])
	}
	// agenteval reads it back as the run's.
	res, err := agenteval.ResultAt(t.Context(), storeOf(t, sess), sess.ID(), "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Snapshot == nil || res.Snapshot.Digest != s.Digest || res.Snapshot.Error != "" {
		t.Errorf("agenteval.ResultAt's snapshot %+v, want the entry's", res.Snapshot)
	}
	// The env entry names the commit the run started from.
	var env *agentsession.EnvEntry
	for _, e := range path {
		if v, ok := e.(*agentsession.EnvEntry); ok {
			env = v
		}
	}
	if env == nil {
		t.Fatal("the run has no env entry")
	}
	if v := env.VCS; v == nil || v.System != "git" || v.Revision != base || v.Dirty {
		t.Errorf("the env entry's vcs %+v, want git at %s, clean", v, base)
	}

	// The patch applied to the commit gives the tree.
	clone := t.TempDir()
	snapGit(t, clone, "clone", "-q", dir, ".")
	snapGit(t, clone, "checkout", "-q", base)
	patch := filepath.Join(t.TempDir(), "final.diff")
	if err := os.WriteFile(patch, r.Data, 0o600); err != nil {
		t.Fatal(err)
	}
	snapGit(t, clone, "apply", "--binary", patch)
	want := treeOf(t, dir, "ignored")
	if _, ok := want["sub/new.txt"]; !ok || want["untracked"] != "u\n" {
		t.Fatalf("the run left %v", want)
	}
	if _, ok := want["gone"]; ok {
		t.Fatal("the run did not delete gone")
	}
	if have := treeOf(t, clone); !equalTrees(have, want) {
		t.Errorf("the patch applied gives %v, want %v", have, want)
	}
	if strings.Contains(string(r.Data), "secret") {
		t.Error("the patch holds the ignored file")
	}
}

func equalTrees(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// storeOf is a store that reads the session's record, as agenteval
// reads a run's: through Read alone.
func storeOf(t *testing.T, sess *agent.Session) agentsession.Store {
	t.Helper()
	return readStore{rec: recordOf(t, sess)}
}

type readStore struct {
	agentsession.Store
	rec *agentsession.Session
}

func (r readStore) Read(context.Context, string) (*agentsession.Session, error) { return r.rec, nil }

// A tree dirty at the start gets a start snapshot beside the final
// one, its base saying it is the start, and the env entry says it was
// dirty; reading it leaves the repository's index and its work tree as
// they were, byte for byte.
func TestPrintSnapshotLeavesTheRepositoryAlone(t *testing.T) {
	dir := t.TempDir()
	base := snapshotRepo(t, dir)
	// A staged change, an unstaged one, an untracked file and an
	// ignored one.
	os.WriteFile(filepath.Join(dir, "keep"), []byte("a2\n"), 0o644)
	snapGit(t, dir, "add", "keep")
	os.WriteFile(filepath.Join(dir, "mod"), []byte("c3\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "untracked"), []byte("u\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "ignored"), []byte("i\n"), 0o644)
	index, err := os.ReadFile(filepath.Join(dir, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	tree := treeOf(t, dir)
	status := snapGit(t, dir, "status", "--porcelain")

	sess := snapshotSession(t, dir, &steps{})
	got, stderr := runSnapshot(t, sess, true, 0)
	if stderr != "" {
		t.Errorf("dax said %q", stderr)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, ".git", "index")); !bytes.Equal(after, index) {
		t.Error("the snapshot changed the repository's index")
	}
	if after := treeOf(t, dir); !equalTrees(after, tree) {
		t.Errorf("the snapshot changed the work tree: %v, was %v", after, tree)
	}
	if after := snapGit(t, dir, "status", "--porcelain"); after != status {
		t.Errorf("git status after: %q, was %q", after, status)
	}
	if got.StartSnapshot == nil || got.StartSnapshot.Base != agent.StartBase+base || got.StartSnapshot.Error != "" {
		t.Fatalf("-result's start snapshot %+v, want base %s", got.StartSnapshot, agent.StartBase+base)
	}
	if got.Snapshot == nil || got.Snapshot.Base != base || got.Snapshot.Digest != got.StartSnapshot.Digest {
		t.Errorf("a run that changed nothing: final %+v, want the start's patch against %s", got.Snapshot, base)
	}
	snaps, ids := snapshotsOn(recordOf(t, sess))
	if len(ids) != 2 || ids[0] != got.StartSnapshot.Entry || ids[1] != got.Snapshot.Entry {
		t.Fatalf("snapshots %v, want the start's and then the final", ids)
	}
	for _, want := range []string{"+a2", "+c3", "+u"} {
		if !strings.Contains(string(snaps[ids[0]].Data), want) {
			t.Errorf("the start's patch lacks %q:\n%s", want, snaps[ids[0]].Data)
		}
	}
	for _, e := range recordOf(t, sess).Entries() {
		if v, ok := e.(*agentsession.EnvEntry); ok && (v.VCS == nil || !v.VCS.Dirty || v.VCS.Revision != base) {
			t.Errorf("the env entry's vcs %+v, want git at %s, dirty", v.VCS, base)
		}
	}
}

// No snapshot is written outside a git tree, without a word, nor with
// -snapshot=false in one; a patch over the bound is recorded with its
// size and no data.
func TestPrintSnapshotWhenNone(t *testing.T) {
	t.Run("not git", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("no git")
		}
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "f"), []byte("x\n"), 0o644)
		sess := snapshotSession(t, dir, &steps{})
		got, stderr := runSnapshot(t, sess, true, 0)
		if stderr != "" || got.Snapshot != nil || got.StartSnapshot != nil {
			t.Errorf("outside git: said %q, result %+v %+v, want nothing", stderr, got.Snapshot, got.StartSnapshot)
		}
		if _, ids := snapshotsOn(recordOf(t, sess)); len(ids) != 0 {
			t.Errorf("outside git the record holds snapshots %v", ids)
		}
	})
	t.Run("off", func(t *testing.T) {
		dir := t.TempDir()
		snapshotRepo(t, dir)
		os.WriteFile(filepath.Join(dir, "mod"), []byte("dirty\n"), 0o644)
		sess := snapshotSession(t, dir, &steps{})
		got, _ := runSnapshot(t, sess, false, 0)
		if got.Snapshot != nil || got.StartSnapshot != nil {
			t.Errorf("-snapshot=false: result %+v %+v, want none", got.Snapshot, got.StartSnapshot)
		}
		rec := recordOf(t, sess)
		if _, ids := snapshotsOn(rec); len(ids) != 0 {
			t.Errorf("-snapshot=false: the record holds snapshots %v", ids)
		}
		for _, e := range rec.Entries() {
			if v, ok := e.(*agentsession.EnvEntry); ok && v.VCS != nil {
				t.Errorf("-snapshot=false: the env entry's vcs %+v, want none read", v.VCS)
			}
		}
	})
	t.Run("too large", func(t *testing.T) {
		dir := t.TempDir()
		snapshotRepo(t, dir)
		m := &steps{parent: [][2]string{{"write", `{"path":"big.txt","content":"` + strings.Repeat("x", 4096) + `"}`}}}
		sess := snapshotSession(t, dir, m)
		got, _ := runSnapshot(t, sess, true, 1024)
		s := got.Snapshot
		if s == nil || !strings.HasPrefix(s.Error, "too large") || s.Size <= 4096 || s.Digest != "" {
			t.Fatalf("over the bound: %+v, want too large with its size and no digest", s)
		}
		snaps, _ := snapshotsOn(recordOf(t, sess))
		if r := snaps[s.Entry]; r.Data != nil || r.Error != s.Error {
			t.Errorf("the entry %+v, want no data and the error", r)
		}
	})
}
