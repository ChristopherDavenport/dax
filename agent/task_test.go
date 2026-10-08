package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agentsmd"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/internal/config"
	"github.com/ChristopherDavenport/dax/policy"
)

// taskModels is a scripted parent and a scripted task child, the child
// known by the preamble in its instructions. It keeps the child's
// requests.
type taskModels struct {
	parent, child scripted
	mu            sync.Mutex
	childReqs     []openresponses.Request
}

func (m *taskModels) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	if strings.Contains(req.Instructions, "You are a sub-agent of dax") {
		m.mu.Lock()
		m.childReqs = append(m.childReqs, req)
		m.mu.Unlock()
		return m.child.CreateStream(ctx, req, sink)
	}
	return m.parent.CreateStream(ctx, req, sink)
}

func runTask(t *testing.T, childCalls [][2]string, rules config.Rules, fallback string, approve func(*openresponses.FunctionCall, string) bool) (Options, *taskModels, *Session) {
	t.Helper()
	model := &taskModels{
		parent: scripted{calls: [][2]string{{"task", `{"input":"write hello.txt saying hi"}`}}},
		child:  scripted{calls: childCalls},
	}
	o := options(t, model)
	o.Agents, o.Model, o.SubagentModel = true, "pro", "flash"
	o.Instructions = "Use tabs."
	o.Approve = approve
	p, err := policy.Build(config.PolicySettings{Builtin: true, Fallback: fallback, User: rules})
	if err != nil {
		t.Fatal(err)
	}
	o.Policy = &p
	s, err := New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	return o, model, s
}

func TestATaskSubagentChangesTheProject(t *testing.T) {
	o, model, s := runTask(t, [][2]string{{"write", `{"path":"hello.txt","content":"hi\n"}`}}, config.Rules{}, "allow", nil)
	if data, err := os.ReadFile(filepath.Join(o.Dir, "hello.txt")); err != nil || string(data) != "hi\n" {
		t.Fatalf("hello.txt: %q, %v", data, err)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if len(model.childReqs) == 0 {
		t.Fatal("the task child was never asked")
	}
	req := model.childReqs[0]
	if req.Model != "flash" {
		t.Errorf("the child asked %q, want the sub-agent model", req.Model)
	}
	// dax's prompt, the user's instructions and the project's AGENTS.md
	// reach the child; the memory and skills blocks, whose tools it
	// lacks, do not.
	for _, want := range []string{"You are a sub-agent of dax", "You are dax, a coding agent", "Use tabs.", "Run go test before saying done."} {
		if !strings.Contains(req.Instructions, want) {
			t.Errorf("child instructions lack %q", want)
		}
	}
	// The guide to the sub-agents is the main agent's: the child has
	// no sub-agents of its own.
	for _, not := range []string{"greet", "memory_save", "let sub-agents do"} {
		if strings.Contains(req.Instructions, not) {
			t.Errorf("child instructions carry %q", not)
		}
	}
	if !strings.Contains(s.Agent.Config().Instructions, "let sub-agents do") {
		t.Error("the main agent's instructions lack the guide to its sub-agents")
	}
	var names []string
	for _, tl := range req.Tools {
		if f, ok := tl.(*openresponses.FunctionTool); ok {
			names = append(names, f.Name)
		}
	}
	if got := strings.Join(names, ","); got != "read,write,edit,glob,grep,ls,bash" {
		t.Errorf("child tools %s; want the built-ins and no sub-agents of its own", got)
	}
	// The AGENTS.md text the child is told is the part the kit renders
	// for the main agent, byte for byte.
	for _, p := range s.Kit.Parts() {
		if p.ID == agentsmd.PartID && (p.Text == "" || !strings.Contains(req.Instructions, p.Text)) {
			t.Errorf("the child's AGENTS.md text is not the kit's part %q", p.Text)
		}
	}
	// The parent sees the child's answer as the task's output.
	if out := outputs(s); len(out) == 0 || !strings.Contains(out[len(out)-1], "done") {
		t.Errorf("parent outputs %q", out)
	}
}

func TestATaskSubagentIsGovernedByThePolicy(t *testing.T) {
	// A deny holds in the child.
	o, _, _ := runTask(t, [][2]string{{"write", `{"path":"hello.txt","content":"hi\n"}`}}, config.Rules{Deny: []string{"write"}}, "allow", nil)
	if _, err := os.Stat(filepath.Join(o.Dir, "hello.txt")); err == nil {
		t.Error("a denied write ran in the task child")
	}

	// An ask goes to the user, named as the task sub-agent's.
	var asked []string
	o, _, _ = runTask(t, [][2]string{{"write", `{"path":"hello.txt","content":"hi\n"}`}}, config.Rules{}, "ask", func(c *openresponses.FunctionCall, reason string) bool {
		asked = append(asked, reason)
		return false
	})
	if len(asked) != 1 || !strings.Contains(asked[0], "the task sub-agent asks") {
		t.Errorf("asked %q", asked)
	}
	if _, err := os.Stat(filepath.Join(o.Dir, "hello.txt")); err == nil {
		t.Error("a write the user refused ran")
	}

	// A front that cannot ask (the terminal client) refuses, and says so.
	o, model, _ := runTask(t, [][2]string{{"write", `{"path":"hello.txt","content":"hi\n"}`}}, config.Rules{}, "ask", nil)
	if _, err := os.Stat(filepath.Join(o.Dir, "hello.txt")); err == nil {
		t.Error("an asked write ran with nobody to ask")
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	last := model.childReqs[len(model.childReqs)-1]
	found := false
	for _, it := range last.Input {
		if out, ok := it.(*openresponses.FunctionCallOutput); ok && strings.Contains(out.Output.Text, "the task sub-agent cannot ask you") {
			found = true
		}
	}
	if !found {
		t.Error("the child was not told why its call was refused")
	}
}

func TestNoSubagentsWhenTurnedOff(t *testing.T) {
	o := options(t, &scripted{})
	s, err := New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, n := range toolNames(s) {
		if n == "task" || n == "explore" {
			t.Errorf("%s offered without Agents", n)
		}
	}
}

// The sub-agent's session records a decision for each call the policy
// allowed, as the main agent's does.
func TestATaskRecordsItsPolicyDecisions(t *testing.T) {
	o, _, s := runTask(t, [][2]string{{"write", `{"path":"hello.txt","content":"hi\n"}`}}, config.Rules{}, "allow", nil)
	parent := s.ID()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	sums, err := List(context.Background(), o.Root, o.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, sum := range sums {
		if sum.Header.ParentSession != parent {
			continue
		}
		path, err := Project(context.Background(), o.Root, sum.Header.ID, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(path)
		if !strings.Contains(string(data), `"type":"decision"`) || !strings.Contains(string(data), `"by":"policy"`) {
			t.Errorf("the task session records no policy decision:\n%s", data)
		}
		return
	}
	t.Fatal("no task session")
}

// The model may only name a role and a context the tool offers; anything
// else is refused before a sub-agent starts, and the main agent sees
// why.
func TestTaskArgumentsAreChecked(t *testing.T) {
	for args, want := range map[string]string{
		`{"input":"x","model":"gpt-9"}`:     `model "gpt-9": want subagent or main`,
		`{"input":"x","context":"inherit"}`: `context "inherit": want fresh or fork`,
		`{"input":"  "}`:                    "input is required",
	} {
		model := &taskModels{parent: scripted{calls: [][2]string{{"task", args}}}}
		o := options(t, model)
		o.Agents = true
		s, err := New(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Prompt(context.Background(), "go"); err != nil {
			t.Fatal(err)
		}
		out := outputs(s)
		if len(out) == 0 || !strings.Contains(out[0], want) {
			t.Errorf("%s: outputs %q, want %q", args, out, want)
		}
		model.mu.Lock()
		if len(model.childReqs) != 0 {
			t.Errorf("%s: a sub-agent started", args)
		}
		model.mu.Unlock()
		s.Close()
	}
}

// Ask, the question a running call puts to the user, is how a front
// that can ask one answers a sub-agent's held call; it is preferred to
// Approve, a refusal's note reaches the sub-agent, and a front that
// cannot ask refuses with a reason the model can act on.
func TestASubagentsHeldCallGoesToAsk(t *testing.T) {
	type outcome struct {
		allow bool
		note  string
		err   error
	}
	for name, tc := range map[string]struct {
		ask      outcome
		wrote    bool
		childSaw string
	}{
		"allowed":       {outcome{allow: true}, true, ""},
		"refused":       {outcome{note: "not now"}, false, "Reason: not now"},
		"nobody to ask": {outcome{err: errors.New("no client")}, false, "cannot ask you"},
	} {
		t.Run(name, func(t *testing.T) {
			model := &taskModels{
				parent: scripted{calls: [][2]string{{"task", `{"input":"write hello.txt"}`}}},
				child:  scripted{calls: [][2]string{{"write", `{"path":"hello.txt","content":"hi\\n"}`}}},
			}
			o := options(t, model)
			o.Agents = true
			p, err := policy.Build(config.PolicySettings{Builtin: true, Fallback: "ask"})
			if err != nil {
				t.Fatal(err)
			}
			o.Policy = &p
			o.Approve = func(*openresponses.FunctionCall, string) bool {
				t.Error("Approve was asked while Ask is set")
				return true
			}
			var asked []string
			o.Ask = func(_ context.Context, call *openresponses.FunctionCall, reason string) (bool, string, error) {
				asked = append(asked, call.Name+": "+reason)
				return tc.ask.allow, tc.ask.note, tc.ask.err
			}
			s, err := New(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if _, err := s.Prompt(context.Background(), "go"); err != nil {
				t.Fatal(err)
			}
			if len(asked) != 1 || !strings.HasPrefix(asked[0], "write: the task sub-agent asks") {
				t.Errorf("asked %q", asked)
			}
			_, err = os.Stat(filepath.Join(o.Dir, "hello.txt"))
			if (err == nil) != tc.wrote {
				t.Errorf("hello.txt written: %v, want %v", err == nil, tc.wrote)
			}
			if tc.childSaw != "" {
				model.mu.Lock()
				last := model.childReqs[len(model.childReqs)-1]
				model.mu.Unlock()
				if !strings.Contains(itemsOutputs(last.Input), tc.childSaw) {
					t.Errorf("the sub-agent saw %q, want %q", itemsOutputs(last.Input), tc.childSaw)
				}
			}
		})
	}
}

func itemsOutputs(items openresponses.Items) string {
	var b strings.Builder
	for _, it := range items {
		if o, ok := it.(*openresponses.FunctionCallOutput); ok {
			b.WriteString(o.Output.Text + "\\n")
		}
	}
	return b.String()
}
