package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dex/internal/config"
	"github.com/ChristopherDavenport/dex/internal/policy"
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
	if strings.Contains(req.Instructions, "You are a sub-agent of dex") {
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
	// dex's prompt, the user's instructions and the project's AGENTS.md
	// reach the child; the memory and skills blocks, whose tools it
	// lacks, do not.
	for _, want := range []string{"You are a sub-agent of dex", "You are dex, a coding agent", "Use tabs.", "Run go test before saying done."} {
		if !strings.Contains(req.Instructions, want) {
			t.Errorf("child instructions lack %q", want)
		}
	}
	for _, not := range []string{"greet", "memory_save"} {
		if strings.Contains(req.Instructions, not) {
			t.Errorf("child instructions carry %q", not)
		}
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
