package memory_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/ext/memory"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/policy"
)

// scripted makes the calls in order, one per model call, then answers.
type scripted struct{ calls [][2]string }

func (m *scripted) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	step := 0
	for _, it := range req.Input {
		switch v := it.(type) {
		case *openresponses.Message:
			if v.Role == openresponses.RoleUser {
				step = 0
			}
		case *openresponses.FunctionCallOutput:
			step++
		}
	}
	em := openresponses.NewEmitter(sink, openresponses.NewResponse(req))
	if step < len(m.calls) {
		call, err := em.FunctionCall("", m.calls[step][0])
		if err != nil {
			return err
		}
		if err := call.Arguments(m.calls[step][1]); err != nil {
			return err
		}
		if err := call.Close(); err != nil {
			return err
		}
		return em.Complete()
	}
	msg, err := em.Message(openresponses.PhaseFinalAnswer)
	if err != nil {
		return err
	}
	if err := msg.Text("done"); err != nil {
		return err
	}
	if err := msg.Close(); err != nil {
		return err
	}
	return em.Complete()
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// session is dax-memory over a store in a fresh user directory, under
// the policy's default of asking.
func session(t *testing.T, model openresponses.Streamer) (agent.Options, string) {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "user", "memory")
	o := agent.Options{
		Model: "scripted", Streamer: model,
		Dir: filepath.Join(base, "project"), UserDir: filepath.Join(base, "user"),
		Policy:     &policy.Settings{Builtin: true, Fallback: "ask"},
		Extensions: []extension.Extension{memory.New(dir)},
	}
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return o, dir
}

func TestTheExtension(t *testing.T) {
	e := memory.New("/m")
	want := []string{"memory_save", "memory_patch", "memory_forget", "memory_search"}
	if e.Name != "dax-memory" || !slices.Equal(e.Owns, want) || !slices.Equal(e.Policy.Allow, []string{"memory_search"}) ||
		len(e.Policy.Ask)+len(e.Policy.Deny) != 0 || e.Tools != nil || e.Kit == nil {
		t.Errorf("extension %+v", e)
	}
}

// The memory tools are offered; a search runs unasked, and a save, a
// patch and a forget ask.
func TestASearchRunsUnaskedAndAChangeAsks(t *testing.T) {
	var approve func(*openresponses.FunctionCall, string) bool
	for _, tc := range []struct {
		tool, args string
		asked      bool
	}{
		{"memory_search", `{"query":"tabs"}`, false},
		{"memory_save", `{"scope":"user","name":"style","content":"Use tabs."}`, true},
		{"memory_patch", `{"scope":"user","name":"style","old_text":"tabs","new_text":"spaces"}`, true},
		{"memory_forget", `{"scope":"user","name":"style"}`, true},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			ctx := context.Background()
			o, _ := session(t, &scripted{calls: [][2]string{{tc.tool, tc.args}}})
			var asked []string
			approve = func(c *openresponses.FunctionCall, _ string) bool {
				asked = append(asked, c.Name)
				return false
			}
			s, err := agent.New(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			var names []string
			for _, tl := range s.Tools() {
				names = append(names, tl.Name)
			}
			for _, n := range memory.New("").Owns {
				if !slices.Contains(names, n) {
					t.Errorf("tools %v lack %s", names, n)
				}
			}
			if _, err := prompt(ctx, s, "remember", approve); err != nil {
				t.Fatal(err)
			}
			if got := len(asked) == 1 && asked[0] == tc.tool; got != tc.asked || !tc.asked && len(asked) > 0 {
				t.Errorf("asked about %v, want asked=%v", asked, tc.asked)
			}
		})
	}
}

func TestTheStoreIsMadePrivate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before os.FileMode // 0: absent
		warn   string
	}{
		{"a fresh store is created private and quietly", 0, ""},
		{"a group-readable one is made private, with a warning", 0o750, " was readable by other users (mode 0750)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, dir := session(t, &scripted{})
			if tc.before != 0 {
				if err := os.MkdirAll(dir, tc.before); err != nil {
					t.Fatal(err)
				}
				os.Chmod(dir, tc.before)
			}
			// The warning is held back with the session's own, for a
			// front that is about to take the screen.
			var warned strings.Builder
			restore := agent.CaptureWarnings(&warned)
			s, err := agent.New(context.Background(), o)
			restore()
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
			if m := mode(t, dir); m != 0o700 {
				t.Errorf("%s is %04o, want 0700", dir, m)
			}
			if tc.warn == "" && warned.Len() != 0 || tc.warn != "" && !strings.Contains(warned.String(), dir+tc.warn) {
				t.Errorf("warned %q, want %q", warned.String(), tc.warn)
			}
		})
	}
}

func TestTheProjectScopeIsKebabAndPerDirectory(t *testing.T) {
	a, b := memory.ProjectScope("/home/u/My Project"), memory.ProjectScope("/srv/My Project")
	if a == b {
		t.Errorf("two directories share the scope %q", a)
	}
	for _, tc := range []struct{ dir, prefix string }{
		{"/home/u/My Project", "project-my-project-"},
		{"/x/Ünïcode_dir!!", "project-n-code-dir-"},
		{"/", "project-"},
	} {
		got := string(memory.ProjectScope(tc.dir))
		if !strings.HasPrefix(got, tc.prefix) || len(got) != len(tc.prefix)+8 {
			t.Errorf("ProjectScope(%q) = %q, want %q and 8 hex digits", tc.dir, got, tc.prefix)
		}
	}
}
