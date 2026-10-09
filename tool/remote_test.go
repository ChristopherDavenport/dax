package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/facts/factspolicy"
)

// mapped stands in for a container: its processes and its files see
// the working directory as /workspace, whatever directory on this
// machine holds it. Paths in a command's arguments are mapped in, and
// paths in its output mapped back, as a container's mount does.
type mapped struct {
	backing *workspace.Local
	dir     string // the directory on this machine
}

const mappedRoot = "/workspace"

func newMapped(t *testing.T, dir string) *mapped {
	t.Helper()
	l, err := workspace.NewLocal(dir, DefaultEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return &mapped{backing: l, dir: l.Root()}
}

func (m *mapped) Root() string  { return mappedRoot }
func (m *mapped) FS() fs.FS     { return m.backing.FS() }
func (m *mapped) Env() []string { return m.backing.Env() }
func (m *mapped) Close() error  { return nil }
func (m *mapped) WriteFile(ctx context.Context, name string, data []byte, perm fs.FileMode) error {
	return m.backing.WriteFile(ctx, name, data, perm)
}
func (m *mapped) Remove(ctx context.Context, name string) error { return m.backing.Remove(ctx, name) }
func (m *mapped) Descriptor() workspace.Descriptor {
	return workspace.Descriptor{Kind: workspace.KindContainer, Ref: "sha256:test", Root: mappedRoot}
}

func (m *mapped) Exec(ctx context.Context, c workspace.Command) (*workspace.Output, error) {
	in := func(s string) string { return strings.ReplaceAll(s, mappedRoot, m.dir) }
	out := func(b []byte) []byte { return bytes.ReplaceAll(b, []byte(m.dir), []byte(mappedRoot)) }
	args := make([]string, len(c.Args))
	for i, a := range c.Args {
		args[i] = in(a)
	}
	c.Args = args
	stream := c.Stream
	var buf bytes.Buffer
	if stream != nil {
		c.Stream = &buf
	}
	o, err := m.backing.Exec(ctx, c)
	if err != nil {
		return nil, err
	}
	if stream != nil {
		stream.Write(out(buf.Bytes()))
	}
	o.Stdout, o.Stderr = out(o.Stdout), out(o.Stderr)
	return o, nil
}

// noLinks is a workspace whose FS cannot tell a link from what it leads
// to, as an FS over a plain file API may not.
type noLinks struct{ *workspace.Local }

func (n noLinks) FS() fs.FS { return struct{ fs.FS }{n.Local.FS()} }

// project lays out a small repository: files, a secret, a link to the
// secret, a directory, and a link to a directory outside it.
func remoteProject(t *testing.T) (dir, outside string) {
	t.Helper()
	dir, outside = t.TempDir(), t.TempDir()
	for name, content := range map[string]string{"README.md": "# hi\n", "src/a.go": "package a\n", ".env": "TOKEN=x\n"} {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
	}
	os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o644)
	os.Symlink(".env", filepath.Join(dir, "notes.txt"))
	os.Symlink(outside, filepath.Join(dir, "out"))
	git := exec.Command("git", "init", "-q", dir)
	git.Env = DefaultEnv(nil)
	if b, err := git.CombinedOutput(); err != nil {
		t.Skipf("no git: %v %s", err, b)
	}
	return dir, outside
}

// The tools and the policy's checks behave the same whether the
// workspace is this machine's directory or a container that calls it
// /workspace: a path is the workspace's, written in its own namespace.
func TestTheToolsAndChecksAreTheSameInAContainer(t *testing.T) {
	dir, outside := remoteProject(t)
	local, err := workspace.NewLocal(dir, DefaultEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { local.Close() })
	for _, ws := range []workspace.Workspace{local, newMapped(t, dir)} {
		root := ws.Root()
		t.Run(ws.Descriptor().Kind, func(t *testing.T) {
			f := NewFiles(ws)
			ctx := context.Background()
			abs := func(p string) string { return filepath.Join(root, p) }

			// The file tools, by the workspace's own absolute paths and
			// by relative ones.
			if out, err := call(ctx, Write(f), `{"path":"`+abs("new/x.txt")+`","content":"made"}`); err != nil {
				t.Fatalf("write %s: %v %s", abs("new/x.txt"), err, out)
			}
			if b, _ := os.ReadFile(filepath.Join(dir, "new/x.txt")); string(b) != "made" {
				t.Errorf("write landed %q", b)
			}
			for _, p := range []string{abs("README.md"), "README.md", "./src/../README.md"} {
				if out, err := call(ctx, Read(f), `{"path":"`+p+`"}`); err != nil || !strings.Contains(out, "# hi") {
					t.Errorf("read %s = %q, %v", p, out, err)
				}
			}
			if out, err := call(ctx, Grep(f), `{"pattern":"package","path":"`+abs("src")+`"}`); err != nil || !strings.Contains(out, "src/a.go:1:") {
				t.Errorf("grep = %q, %v", out, err)
			}
			if out, err := call(ctx, Glob(f), `{"pattern":"**/*.go"}`); err != nil || !strings.Contains(out, "src/a.go") {
				t.Errorf("glob = %q, %v", out, err)
			}
			// A path outside the namespace is outside, even one that
			// names the same files on this machine: the container has
			// no such path.
			for _, p := range []string{filepath.Join(outside, "secret"), "out/secret", "../x"} {
				if _, err := call(ctx, Read(f), `{"path":"`+p+`"}`); err == nil || !errors.Is(err, ErrOutside) && !strings.Contains(err.Error(), "outside the workspace") {
					t.Errorf("read %s: err = %v, want outside", p, err)
				}
			}
			if root != dir {
				if _, err := call(ctx, Read(f), `{"path":"`+filepath.Join(dir, "README.md")+`"}`); err == nil {
					t.Errorf("read by this machine's path %s succeeded in a workspace rooted at %s", dir, root)
				}
			}

			// bash runs in the workspace, at its root.
			if out, err := call(ctx, Bash(f), `{"command":"pwd && cat README.md"}`); err != nil || out != root+"\n# hi\n[exit 0]" {
				t.Errorf("bash = %q, %v", out, err)
			}

			// The policy's path subjects, from read's facts claim: the
			// name in the workspace, and what a link leads to.
			subj := factspolicy.Subjects(Read(f), []string{"read"})
			for raw, want := range map[string][]string{
				abs(".env"):  {".env"},
				"notes.txt":  {"notes.txt", ".env"},
				"out/secret": {sentinel + "out/secret"},
			} {
				args, _ := json.Marshal(map[string]string{"path": raw})
				got, err := subj(t.Context(), args)
				if err != nil {
					t.Fatal(err)
				}
				var paths []string
				for _, s := range got {
					var m map[string]string
					json.Unmarshal(s.Args, &m)
					paths = append(paths, m["path"])
				}
				if raw == "out/secret" {
					// A link out: the name, and nothing it leads to.
					if len(got) != 1 || got[0].Tool != "" {
						t.Errorf("subjects of %s = %+v", raw, got)
					}
					continue
				}
				if strings.Join(paths, " ") != strings.Join(want, " ") {
					t.Errorf("subjects of %s = %q, want %q", raw, paths, want)
				}
			}

			// The bash check: what runs unasked, judged by the
			// workspace's files and its git, the same on both.
			an := &Analyzer{Files: f}
			for cmd, auto := range map[string]bool{
				"cat " + abs("README.md"):                 true,
				"cat README.md && ls src":                 true,
				"git status":                              true,
				"git log -n1":                             true,
				"cat " + filepath.Join(outside, "secret"): false,
				"cat out/secret":                          false,
				"ls out/*":                                false,
				"cd out && ls":                            false,
				"rm README.md":                            false,
			} {
				if got := an.Check(ctx, cmd).Auto; got != auto {
					t.Errorf("%q auto = %v, want %v", cmd, got, auto)
				}
				// bash's claim stamps exactly the lines that run unasked.
				args, _ := json.Marshal(map[string]string{"command": cmd})
				fx, _, err := agenttool.FactsOf(ctx, Bash(f), args)
				if err != nil {
					t.Fatal(err)
				}
				if stamped := strings.Contains(string(fx.Rewrite), "dax_stamp"); stamped != auto {
					t.Errorf("%q stamped by its claim = %v, want %v", cmd, stamped, auto)
				}
			}
			// A stamped call runs the plan, in the workspace.
			if out, err := call(ctx, Bash(f), stampedIn(t, f, "cat README.md")); err != nil || out != "# hi\n[exit 0]" {
				t.Errorf("stamped bash = %q, %v", out, err)
			}
		})
	}
}

// stampedIn is the arguments of a bash call in f as the policy hook
// passes them on: the rewrite bash's facts claim asks for.
func stampedIn(t *testing.T, f *Files, cmd string) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"command": cmd})
	fx, _, err := agenttool.FactsOf(context.Background(), Bash(f), raw)
	if err != nil || fx.Rewrite == nil {
		t.Fatalf("%q was not stamped: %v", cmd, err)
	}
	return string(fx.Rewrite)
}

// A workspace that cannot read links cannot say what a path leads to,
// so what Local lets run, because it can, asks there: a read that a
// bare allow rule covers, and a bash line that names a file.
func TestAWorkspaceThatCannotReadLinksAsks(t *testing.T) {
	dir, _ := remoteProject(t)
	local, err := workspace.NewLocal(dir, DefaultEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { local.Close() })
	for _, tc := range []struct {
		name string
		ws   workspace.Workspace
		want agentturn.ToolAction
	}{
		{"local", local, agentturn.Allow},
		{"no links", noLinks{local}, agentturn.Defer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := NewFiles(tc.ws)
			p, err := agentpolicy.Merge(agentpolicy.RuleSet{
				Source: agentpolicy.Source{Name: "test", Trusted: true, Rank: 1},
				Allow:  []agentpolicy.Rule{{Tool: "read"}, {Tool: "bash", Spec: "cat:*"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			p.Default = agentpolicy.Ask()
			ms, err := factspolicy.Matchers([]agenttool.Tool{Read(f), Bash(f)}, []string{"read", "bash"}, map[string]agentpolicy.ToolMatcher{
				"read": {Match: agentpolicy.GlobMatcher("path")},
				"bash": {Match: agentpolicy.GlobMatcher("command")},
			})
			if err != nil {
				t.Fatal(err)
			}
			eng, err := agentpolicy.Build(p, ms)
			if err != nil {
				t.Fatal(err)
			}
			for tool, args := range map[string]string{"read": `{"path":"README.md"}`, "bash": `{"command":"cat README.md"}`} {
				call := &openresponses.FunctionCall{Name: tool, Arguments: args, CallID: "c1"}
				v, err := eng.Would(context.Background(), agentturn.ToolCallInfo{Call: call, Args: json.RawMessage(args), Batch: []*openresponses.FunctionCall{call}})
				if err != nil {
					t.Fatal(err)
				}
				if v.Action != tc.want {
					t.Errorf("%s %s = %v (%s), want %v", tool, args, v.Action, v.Reason, tc.want)
				}
			}
			if got := (&Analyzer{Files: f}).Check(context.Background(), "cat README.md").Auto; got != (tc.want == agentturn.Allow) {
				t.Errorf("cat README.md auto = %v", got)
			}
			// The file tools still work: they read through the
			// workspace, which confines them itself.
			if out, err := call(context.Background(), Read(f), `{"path":"README.md"}`); err != nil || !strings.Contains(out, "# hi") {
				t.Errorf("read = %q, %v", out, err)
			}
		})
	}
}

// fs.Glob passes over a directory it cannot read. Over the confined FS
// a link out of the workspace is one, and bash would follow it: a glob
// through it is refused, not taken for one that matched nothing.
func TestAGlobThroughALinkOutIsRefused(t *testing.T) {
	dir, outside := remoteProject(t)
	for _, n := range []string{"a", "b"} {
		os.WriteFile(filepath.Join(outside, n), []byte("x"), 0o644)
	}
	an := &Analyzer{Files: newWS(t, dir)}
	for _, cmd := range []string{"ls out/*", "ls out/a*", "ls src/../out/*", "ls o*/*"} {
		if c := an.Check(context.Background(), cmd); c.Auto {
			t.Errorf("%q is auto-allowed; it lists %s", cmd, outside)
		}
	}
	if !an.Check(context.Background(), "ls src/*").Auto {
		t.Error("ls src/* asks; a glob that stays inside should run")
	}
}
