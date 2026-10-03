package tool

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool"
)

// call runs one tool with raw arguments and returns the text the model
// would see.
func call(ctx context.Context, t agenttool.Tool, args string) (string, error) {
	res, err := t.Execute(ctx, agenttool.Call{ID: "call_1", Args: json.RawMessage(args)})
	if err != nil {
		return "", err
	}
	return Text(res), nil
}

// newWS opens dir as a workspace for the test.
func newWS(t testing.TB, dir string) *Workspace {
	t.Helper()
	ws, err := NewWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	return ws
}

func TestEdit(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		args    string
		want    string // file content after, when no error
		wantErr string
	}{
		{"replaces one", "a\nb\nc\n", `{"path":"f","old_string":"b","new_string":"B"}`, "a\nB\nc\n", ""},
		{"not found", "a\nb\n", `{"path":"f","old_string":"z","new_string":"Z"}`, "", "not found"},
		{"ambiguous", "a\na\n", `{"path":"f","old_string":"a","new_string":"A"}`, "", "matches 2 times"},
		{"empty old", "a\n", `{"path":"f","old_string":"","new_string":"A"}`, "", "must not be empty"},
		{"missing file", "", `{"path":"nope","old_string":"a","new_string":"b"}`, "", "no such file"},
		{"missing property", "a\n", `{"path":"f","old_string":"a"}`, "", "new_string"},
		{"wrong type", "a\n", `{"path":"f","old_string":"a","new_string":3}`, "", "new_string"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.src != "" {
				if err := os.WriteFile(filepath.Join(dir, "f"), []byte(tc.src), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			_, err := call(context.Background(), Edit(newWS(t, dir)), tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(filepath.Join(dir, "f"))
			if string(got) != tc.want {
				t.Fatalf("file = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReadOffsetLimit(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f"), []byte("one\ntwo\nthree\nfour\n"), 0o644)
	out, err := call(context.Background(), Read(newWS(t, dir)), `{"path":"f","offset":2,"limit":2}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"2\ttwo", "3\tthree", "1 more lines; use offset=4"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "one") || strings.Contains(out, "four") {
		t.Errorf("output has lines outside the window:\n%s", out)
	}
}

func TestSchemas(t *testing.T) {
	// The reflected schemas name the optional fields as optional and the
	// rest as required, so a model that omits a required one is told so
	// before the function runs.
	for _, tc := range []struct {
		tool     agenttool.Tool
		required []string
		optional []string
	}{
		{Read(newWS(t, t.TempDir())), []string{"path"}, []string{"offset", "limit"}},
		{Write(newWS(t, t.TempDir())), []string{"path", "content"}, nil},
		{Edit(newWS(t, t.TempDir())), []string{"path", "old_string", "new_string"}, nil},
		{Glob(newWS(t, t.TempDir())), []string{"pattern"}, []string{"path", "max_results"}},
		{Grep(newWS(t, t.TempDir())), []string{"pattern"}, []string{"path", "include", "ignore_case", "max_results"}},
		{LS(newWS(t, t.TempDir())), nil, []string{"path"}},
		{Bash(t.TempDir()), []string{"command"}, []string{"timeout_seconds"}},
	} {
		var s struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		}
		if err := json.Unmarshal(tc.tool.Parameters(), &s); err != nil {
			t.Fatalf("%s: %v", tc.tool.Name(), err)
		}
		req := strings.Join(s.Required, ",")
		if req != strings.Join(tc.required, ",") {
			t.Errorf("%s: required = %q, want %q", tc.tool.Name(), req, tc.required)
		}
		for _, p := range append(tc.required, tc.optional...) {
			if _, ok := s.Properties[p]; !ok {
				t.Errorf("%s: schema lacks property %q:\n%s", tc.tool.Name(), p, tc.tool.Parameters())
			}
		}
	}
	if !agenttool.IsSequential(Bash(t.TempDir())) {
		t.Error("bash should be sequential")
	}
}

func TestWriteCreatesDirs(t *testing.T) {
	dir := t.TempDir()
	_, err := call(context.Background(), Write(newWS(t, dir)), `{"path":"a/b/c.txt","content":"hi"}`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "a", "b", "c.txt"))
	if err != nil || string(got) != "hi" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestBash(t *testing.T) {
	b := Bash(t.TempDir())
	run := func(ctx context.Context, args string) (string, error) {
		return call(ctx, b, args)
	}

	t.Run("exit code and output", func(t *testing.T) {
		out, err := run(context.Background(), `{"command":"echo hi; echo err >&2; exit 3"}`)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"hi\n", "err\n", "[exit 3]"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q in %q", want, out)
			}
		}
	})

	t.Run("timeout kills children", func(t *testing.T) {
		start := time.Now()
		out, err := run(context.Background(), `{"command":"sleep 30 & wait","timeout_seconds":1}`)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "[killed after 1s]") {
			t.Errorf("want kill note, got %q", out)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("took %s; process group was not killed", d)
		}
	})

	t.Run("cancel returns ctx error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(100 * time.Millisecond); cancel() }()
		_, err := run(ctx, `{"command":"sleep 30"}`)
		if err != context.Canceled {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
}

// The repository's own configuration must not run a program under a
// read-only git command: fsmonitor, a pager, an external diff and a
// textconv driver are all programs a .git/config can name.
func TestReadOnlyGitDoesNotRunTheRepositorysPrograms(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	probe := filepath.Join(t.TempDir(), "ran")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	os.WriteFile(filepath.Join(dir, "f"), []byte("a\n"), 0o644)
	run("add", "f")
	run("-c", "user.name=n", "-c", "user.email=e@x", "commit", "-qm", "x")
	os.WriteFile(filepath.Join(dir, "f"), []byte("b\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("f diff=x\n"), 0o644)
	touch := "sh -c 'touch " + probe + "'"
	run("config", "core.fsmonitor", touch+"; echo")
	run("config", "diff.external", touch+"; true #")
	run("config", "core.pager", touch)
	run("config", "diff.x.textconv", touch+"; cat #")

	b := Bash(dir)
	for _, c := range []string{"git status", "git diff", "git log -p", "git show HEAD", "git diff --stat"} {
		out, err := call(context.Background(), b, `{"command":"`+c+`"}`)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "[exit 0]") {
			t.Errorf("%s: %s", c, out)
		}
		if _, err := os.Stat(probe); err == nil {
			t.Fatalf("%s ran a program named by the repository's config", c)
		}
	}
	// The diff is still a diff.
	out, _ := call(context.Background(), b, `{"command":"git diff"}`)
	if !strings.Contains(out, "-a") || !strings.Contains(out, "+b") {
		t.Errorf("git diff output: %s", out)
	}
}

func TestACommandOutsideTheSubsetStillRunsInBash(t *testing.T) {
	out, err := call(context.Background(), Bash(t.TempDir()), `{"command":"echo a && echo b | tr b c"}`)
	if err != nil || !strings.Contains(out, "a\nc\n") {
		t.Fatalf("%q, %v", out, err)
	}
}

// A signed commit's %GG runs gpg.program. The auto-allow environment
// replaces it, so a program a hostile .git/config names does not run.
func TestGitEnvSwitchesOffTheGPGPrograms(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	probe := filepath.Join(t.TempDir(), "gpg-ran")
	script := filepath.Join(t.TempDir(), "evilgpg")
	os.WriteFile(script, []byte("#!/bin/sh\ntouch "+probe+"\n"), 0o755)
	git := func(env []string, stdin string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null"), env...)
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(nil, "", "init", "-q")
	tree := git(nil, "", "write-tree")
	commit := "tree " + tree + "\nauthor n <e@x> 1 +0000\ncommitter n <e@x> 1 +0000\ngpgsig -----BEGIN PGP SIGNATURE-----\n \n fake\n -----END PGP SIGNATURE-----\n\nmsg\n"
	id := git(nil, commit, "hash-object", "-t", "commit", "-w", "--stdin")
	git(nil, "", "update-ref", "HEAD", id)
	git(nil, "", "config", "gpg.program", script)
	git(nil, "", "config", "gpg.openpgp.program", script)

	git(nil, "", "log", "--format=%GG", "-1")
	if _, err := os.Stat(probe); err != nil {
		t.Skip("this git does not run gpg.program for the signature placeholders; nothing to prove")
	}
	os.Remove(probe)
	git(GitEnv(), "", "log", "--format=%GG", "-1")
	if _, err := os.Stat(probe); err == nil {
		t.Fatal("gpg.program ran under GitEnv")
	}
}
