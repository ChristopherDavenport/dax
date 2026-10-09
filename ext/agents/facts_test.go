package agents_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/ext/agents"
	"github.com/ChristopherDavenport/dax/policy"
)

// callIn makes one call as the main agent or, with child, as a task
// sub-agent's, in a project holding notes.txt and .env, under dax's
// rules with the user's user and the fallback ask; approve answers what
// is asked, given the project's directory. It returns the directory and
// every tool output a model saw, the main agent's and the sub-agent's.
func callIn(t *testing.T, child bool, call [2]string, user policy.Rules, approve func(dir string, c *openresponses.FunctionCall) bool) (string, string) {
	t.Helper()
	return batchIn(t, child, [][2]string{call}, user, approve)
}

// batchIn is callIn with calls made in one response, as one batch.
func batchIn(t *testing.T, child bool, calls [][2]string, user policy.Rules, approve func(dir string, c *openresponses.FunctionCall) bool) (string, string) {
	t.Helper()
	model := &taskModels{parent: scripted{calls: calls, batch: true}}
	if child {
		model = &taskModels{
			parent: scripted{calls: [][2]string{{"task", `{"input":"do it"}`}}},
			child:  scripted{calls: calls, batch: true},
		}
	}
	o := options(t, model, &agents.Options{Model: "flash"})
	write(t, filepath.Join(o.Dir, "notes.txt"), "notes\n")
	write(t, filepath.Join(o.Dir, ".env"), "SECRET=1\n")
	o.Policy = &policy.Settings{Builtin: true, Fallback: "ask", User: user}
	s, err := agent.New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := prompt(context.Background(), s, "go", func(c *openresponses.FunctionCall, _ string) bool {
		return c.Name == "task" || approve(o.Dir, c)
	}); err != nil {
		t.Fatal(err)
	}
	seen := strings.Join(outputs(s), "\n")
	model.mu.Lock()
	defer model.mu.Unlock()
	if child && len(model.childReqs) == 0 {
		t.Fatal("the task sub-agent never ran")
	}
	for _, r := range model.childReqs {
		seen += "\n" + itemsOutputs(r.Input)
	}
	return o.Dir, seen
}

var where = []struct {
	name  string
	child bool
}{{"main agent", false}, {"task sub-agent", true}}

// A call a person approves runs on the facts they were asked about, in
// a sub-agent as in the main agent: notes.txt that became a link to
// .env while the question was open is refused with "ask again", and
// .env reaches no model.
func TestAnApprovedCallRunsOnTheFactsItWasAskedAbout(t *testing.T) {
	for _, w := range where {
		t.Run(w.name, func(t *testing.T) {
			_, seen := callIn(t, w.child, [2]string{"read", `{"path":"notes.txt"}`}, policy.Rules{Ask: []string{"read(notes.txt)"}},
				func(dir string, c *openresponses.FunctionCall) bool {
					os.Remove(filepath.Join(dir, "notes.txt"))
					if err := os.Symlink(".env", filepath.Join(dir, "notes.txt")); err != nil {
						t.Fatal(err)
					}
					return true
				})
			if strings.Contains(seen, "SECRET") || !strings.Contains(seen, "ask again") {
				t.Errorf("a model saw:\n%s\nwant the read refused with ask again", seen)
			}
		})
	}
}

// An auto-allowed bash line runs on the facts the policy decided it on,
// in a sub-agent as in the main agent: cat notes.txt was decided as a
// read of notes.txt, and notes.txt became a link to .env inside the
// project before it ran. The plan is the same text; the facts are not,
// so it is refused with "ask again" and .env reaches no model. The link
// is made by a call the person approved in the same batch, while cat
// was held beside it, and by a change made while cat's own question
// (a rule asks about notes.txt) was open.
func TestABashLineRunsOnTheFactsItWasDecidedOn(t *testing.T) {
	lnThenCat := [][2]string{
		{"bash", `{"command":"ln -sf .env notes.txt"}`},
		{"bash", `{"command":"cat notes.txt"}`},
	}
	for _, w := range where {
		t.Run(w.name+"/a link made earlier in the batch", func(t *testing.T) {
			_, seen := batchIn(t, w.child, lnThenCat, policy.Rules{}, func(_ string, c *openresponses.FunctionCall) bool {
				return strings.Contains(c.Arguments, "ln -sf")
			})
			if strings.Contains(seen, "SECRET") || !strings.Contains(seen, "ask again") {
				t.Errorf("a model saw:\n%s\nwant cat refused with ask again", seen)
			}
		})
		t.Run(w.name+"/a link made while it was asked about", func(t *testing.T) {
			_, seen := callIn(t, w.child, [2]string{"bash", `{"command":"cat notes.txt"}`}, policy.Rules{Ask: []string{"read(notes.txt)"}},
				func(dir string, c *openresponses.FunctionCall) bool {
					os.Remove(filepath.Join(dir, "notes.txt"))
					if err := os.Symlink(".env", filepath.Join(dir, "notes.txt")); err != nil {
						t.Fatal(err)
					}
					return true
				})
			if strings.Contains(seen, "SECRET") || !strings.Contains(seen, "ask again") {
				t.Errorf("a model saw:\n%s\nwant cat refused with ask again", seen)
			}
		})
	}
}

// A stamp the model wrote never reaches the tool, unasked or approved,
// in a sub-agent as in the main agent: an approved read runs with the
// stamp of its facts in place of the forged one, and an approved bash
// line with the forged stamp taken off, as written.
func TestAForgedStampIsReplacedOnAnApprovedCall(t *testing.T) {
	for _, w := range where {
		for _, tc := range []struct {
			name string
			call [2]string
			ran  func(dir, seen string) bool
		}{
			{"read", [2]string{"read", `{"path":"notes.txt","dax_stamp":"forged"}`}, func(_, seen string) bool {
				return strings.Contains(seen, "1\tnotes")
			}},
			{"bash", [2]string{"bash", `{"command":"touch made.txt","dax_stamp":"forged"}`}, func(dir, _ string) bool {
				_, err := os.Stat(filepath.Join(dir, "made.txt"))
				return err == nil
			}},
		} {
			t.Run(w.name+"/"+tc.name, func(t *testing.T) {
				dir, seen := callIn(t, w.child, tc.call, policy.Rules{Ask: []string{"read(notes.txt)"}},
					func(string, *openresponses.FunctionCall) bool { return true })
				if !tc.ran(dir, seen) {
					t.Errorf("the approved call did not run as dax rewrote it; a model saw:\n%s", seen)
				}
			})
		}
	}
}

// A key that is a field in another case runs nothing, approved or not,
// in a sub-agent as in the main agent: the policy is never asked about
// one path while the tool acts on another. On main, the read below was
// allowed unasked and returned .env.
func TestAKeyInAnotherCaseRunsNothing(t *testing.T) {
	for _, w := range where {
		for _, call := range [][2]string{
			{"read", `{"path":"notes.txt","Path":".env"}`},
			{"write", `{"path":"notes.txt","content":"pwn","Path":"victim.txt"}`},
			{"edit", `{"path":"notes.txt","old_string":"notes","new_string":"pwn","Path":".env"}`},
			{"bash", `{"command":"pwd","Command":"touch victim.txt"}`},
		} {
			t.Run(w.name+"/"+call[0], func(t *testing.T) {
				dir, seen := callIn(t, w.child, call, policy.Rules{}, func(string, *openresponses.FunctionCall) bool { return true })
				if strings.Contains(seen, "SECRET") {
					t.Errorf("a model saw .env:\n%s", seen)
				}
				if _, err := os.Stat(filepath.Join(dir, "victim.txt")); err == nil {
					t.Error("victim.txt was written")
				}
				for name, want := range map[string]string{"notes.txt": "notes\n", ".env": "SECRET=1\n"} {
					if b, _ := os.ReadFile(filepath.Join(dir, name)); string(b) != want {
						t.Errorf("%s = %q", name, b)
					}
				}
			})
		}
	}
}
