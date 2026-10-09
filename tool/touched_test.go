package tool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/facts/factspolicy"
)

// stampedBy is the arguments a call of t runs with once the policy
// allowed it: what the session's hook makes of args.
func stampedBy(t *testing.T, tl agenttool.Tool, args string) string {
	t.Helper()
	d, err := factspolicy.Hook([]agenttool.Tool{tl})(context.Background(), hookInfo(tl.Name(), args))
	if err != nil || d == nil || !strings.Contains(string(d.Args), "dax_stamp") {
		t.Fatalf("%s %s not stamped: %+v %v", tl.Name(), args, d, err)
	}
	return string(d.Args)
}

// The race between the facts a call was decided on and the call: a
// file the policy saw as notes.txt becomes a link to .env before the
// call runs. os.Root follows it, since .env is inside; the stamp of the
// facts refuses it. An unchanged call runs; a stamp the model forged is
// refused at the call and replaced by the hook; with no stamp (the
// policy off) nothing is checked.
func TestAFileToolRunsOnlyOnTheFactsItWasAllowedOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		tool func(*Files) agenttool.Tool
		args string
		ran  func(t *testing.T, dir, out string)
	}{
		{"read", func(f *Files) agenttool.Tool { return Read(f) }, `{"path":"notes.txt"}`, func(t *testing.T, _, out string) {
			if !strings.Contains(out, "notes") {
				t.Errorf("read = %q", out)
			}
		}},
		{"write", func(f *Files) agenttool.Tool { return Write(f) }, `{"path":"notes.txt","content":"new"}`, func(t *testing.T, dir, _ string) {
			if b, _ := os.ReadFile(filepath.Join(dir, ".env")); string(b) != "SECRET=1\n" {
				t.Errorf(".env = %q: the write went through the link", b)
			}
		}},
		{"edit", func(f *Files) agenttool.Tool { return Edit(f) }, `{"path":"notes.txt","old_string":"notes","new_string":"x"}`, func(t *testing.T, dir, _ string) {
			if b, _ := os.ReadFile(filepath.Join(dir, ".env")); string(b) != "SECRET=1\n" {
				t.Errorf(".env = %q: the edit went through the link", b)
			}
		}},
		{"grep", func(f *Files) agenttool.Tool { return Grep(f) }, `{"pattern":"notes","path":"notes.txt"}`, func(t *testing.T, _, out string) {
			if !strings.Contains(out, "notes") {
				t.Errorf("grep = %q", out)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			setup := func(t *testing.T) (string, *Files) {
				dir := t.TempDir()
				os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("notes\n"), 0o644)
				os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=1\n"), 0o644)
				return dir, newWS(t, dir)
			}
			swap := func(t *testing.T, dir string) {
				os.Remove(filepath.Join(dir, "notes.txt"))
				if err := os.Symlink(".env", filepath.Join(dir, "notes.txt")); err != nil {
					t.Fatal(err)
				}
			}

			// Unchanged since it was allowed: it runs.
			dir, f := setup(t)
			out, err := call(ctx, tc.tool(f), stampedBy(t, tc.tool(f), tc.args))
			if err != nil {
				t.Fatalf("unchanged call: %v", err)
			}
			tc.ran(t, dir, out)

			// The path became a link to .env between the facts and the call.
			dir, f = setup(t)
			stamped := stampedBy(t, tc.tool(f), tc.args)
			swap(t, dir)
			if out, err := call(ctx, tc.tool(f), stamped); !errors.Is(err, errTouched) {
				t.Errorf("after the swap = %q, %v; want errTouched", out, err)
			}
			if b, _ := os.ReadFile(filepath.Join(dir, ".env")); string(b) != "SECRET=1\n" {
				t.Errorf(".env changed: %q", b)
			}

			// A stamp the model supplied is no stamp of these facts.
			var m map[string]any
			json.Unmarshal([]byte(tc.args), &m)
			m["dax_stamp"] = "forged"
			forged, _ := json.Marshal(m)
			_, f = setup(t)
			if _, err := call(ctx, tc.tool(f), string(forged)); !errors.Is(err, errTouched) {
				t.Errorf("a forged stamp ran: %v", err)
			}
			if got := stampedBy(t, tc.tool(f), string(forged)); strings.Contains(got, "forged") {
				t.Errorf("the hook kept the forged stamp: %s", got)
			}

			// No stamp, as with the policy off: nothing is checked, and the
			// link is followed inside the workspace as before.
			dir, f = setup(t)
			swap(t, dir)
			if _, err := call(ctx, tc.tool(f), tc.args); errors.Is(err, errTouched) {
				t.Errorf("an unstamped call was checked: %v", err)
			}
		})
	}
}

func hookInfo(name, args string) agentturn.ToolCallInfo {
	return agentturn.ToolCallInfo{Call: &openresponses.FunctionCall{Name: name, Arguments: args}, Args: json.RawMessage(args)}
}

// The same race for bash: the policy decided an auto-allowed line on
// what it reads, notes.txt, and a path on its way then became a link to
// a secret inside the workspace. The plan is the same text, so a stamp
// of the plan alone would let it run and read the secret, which a read
// of it asks for; the stamp binds the facts too, so the call is refused
// with "ask again". An unchanged line runs; a stamp the model forged is
// refused; with no stamp (the policy off) the line runs as typed.
func TestABashLineRunsOnlyOnTheFactsItWasAllowedOn(t *testing.T) {
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("SECRET=0\n"), 0o644)
	link := func(target, name string) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) {
			t.Helper()
			if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, tc := range []struct {
		name, cmd string
		swap      func(t *testing.T, dir string)
	}{
		{"a read becomes .env", "cat notes.txt", link(".env", "notes.txt")},
		{"a read in a pipeline becomes .env", "cat notes.txt | head -n 5", link(".env", "notes.txt")},
		{"a head becomes .env", "head -n 1 notes.txt", link(".env", "notes.txt")},
		{"a directory cd enters becomes another", "cd sub && cat notes.txt", link("secret", "sub")},
		{"a read becomes a link out of the workspace", "cat notes.txt", link(filepath.Join(outside, "secret"), "notes.txt")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			setup := func(t *testing.T) (string, *Files) {
				dir := t.TempDir()
				os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("notes\n"), 0o644)
				os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=1\n"), 0o644)
				os.Mkdir(filepath.Join(dir, "sub"), 0o755)
				os.WriteFile(filepath.Join(dir, "sub", "notes.txt"), []byte("notes\n"), 0o644)
				os.Mkdir(filepath.Join(dir, "secret"), 0o755)
				os.WriteFile(filepath.Join(dir, "secret", "notes.txt"), []byte("SECRET=2\n"), 0o644)
				return dir, newWS(t, dir)
			}
			args, _ := json.Marshal(map[string]string{"command": tc.cmd})

			// Unchanged since it was allowed: it runs.
			_, f := setup(t)
			out, err := call(ctx, Bash(f), stampedBy(t, Bash(f), string(args)))
			if err != nil || !strings.Contains(out, "notes") || !strings.Contains(out, "[exit 0]") {
				t.Fatalf("unchanged line = %q, %v", out, err)
			}

			// A path became a link between the facts and the call.
			dir, f := setup(t)
			stamped := stampedBy(t, Bash(f), string(args))
			tc.swap(t, dir)
			out, err = call(ctx, Bash(f), stamped)
			if !errors.Is(err, errChanged) || !strings.Contains(err.Error(), "ask again") {
				t.Errorf("after the swap = %q, %v; want errChanged", out, err)
			}
			if strings.Contains(out, "SECRET") {
				t.Errorf("the secret reached the output: %q", out)
			}

			// A stamp the model supplied is no stamp of this line.
			_, f = setup(t)
			forged, _ := json.Marshal(map[string]string{"command": tc.cmd, "dax_stamp": "forged"})
			if out, err := call(ctx, Bash(f), string(forged)); !errors.Is(err, errChanged) {
				t.Errorf("a forged stamp ran: %q, %v", out, err)
			}

			// No stamp, as with the policy off: the line runs as typed.
			dir, f = setup(t)
			tc.swap(t, dir)
			if _, err := call(ctx, Bash(f), string(args)); err != nil {
				t.Errorf("an unstamped line was checked: %v", err)
			}
		})
	}
}

// A line that writes through a redirect is outside the safe subset, so
// bash's claim never stamps it: it never runs unasked, and there is no
// decision taken without a person for a swap of its target to outrun.
// What a person approves runs as typed (see command).
func TestABashLineThatWritesIsNeverStamped(t *testing.T) {
	dir := t.TempDir()
	f := newWS(t, dir)
	for _, cmd := range []string{"echo x > out/log", "cat notes.txt > out/log", "cat notes.txt >> out/log"} {
		args, _ := json.Marshal(map[string]string{"command": cmd})
		fx, _, err := agenttool.FactsOf(context.Background(), Bash(f), args)
		if err != nil {
			t.Fatal(err)
		}
		if fx.Rewrite != nil {
			t.Errorf("%q was stamped: %s", cmd, fx.Rewrite)
		}
		var sentinelled bool
		for _, c := range fx.Calls {
			sentinelled = sentinelled || strings.Contains(string(c.Args), sentinel)
		}
		if !sentinelled {
			t.Errorf("%q has no subject that keeps every allow rule off it: %+v", cmd, fx.Calls)
		}
	}
}
