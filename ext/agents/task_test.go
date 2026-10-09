package agents_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentmemory/filestore"
	"github.com/ChristopherDavenport/agentsmd"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/ext/agents"
	"github.com/ChristopherDavenport/dax/ext/coding"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/policy"
)

// scripted makes one call per step of a run, the step being the number
// of tool outputs since the last user message, then answers "done".
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

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// layers is a test extension with a skill catalogue and a memory store,
// added through the kit as dax-skills and dax-memory add them, so a
// test can see that neither reaches a sub-agent.
func layers(userDir string) extension.Extension {
	return extension.Extension{
		Name: "test-layers",
		Owns: []string{"skill", "memory_save", "memory_patch", "memory_forget", "memory_search"},
		Kit: func(extension.Env) ([]agentkit.Option, error) {
			mem, err := filestore.Open(filepath.Join(userDir, "memory"))
			if err != nil {
				return nil, err
			}
			return []agentkit.Option{agentkit.WithOptionalSkills(filepath.Join(userDir, "skills")), agentkit.WithMemory(mem, "user")}, nil
		},
	}
}

// options is a session in a fresh project, user directory and store,
// on the given model, with dax-coding, AGENTS.md, a skill and memory,
// and dax-agents when sub is not nil.
func options(t *testing.T, model openresponses.Streamer, sub *agents.Options) agent.Options {
	t.Helper()
	base := t.TempDir()
	o := agent.Options{
		Model:    "echo",
		Streamer: model,
		Dir:      filepath.Join(base, "project"),
		Root:     filepath.Join(base, "sessions"),
		UserDir:  filepath.Join(base, "user"),
		AgentsMD: true,
	}
	write(t, filepath.Join(o.Dir, "AGENTS.md"), "Run go test before saying done.\n")
	write(t, filepath.Join(o.UserDir, "skills", "greet", "SKILL.md"),
		"---\nname: greet\ndescription: How to greet the user.\nallowed-tools: Bash(echo:*)\n---\nSay hello.\n")
	o.Extensions = []extension.Extension{coding.New(0)}
	if sub != nil {
		o.Extensions = append(o.Extensions, agents.New(*sub))
	}
	o.Extensions = append(o.Extensions, layers(o.UserDir))
	return o
}

// outputs are the function-call outputs in a transcript, in order.
func outputs(s *agent.Session) []string {
	var out []string
	for _, it := range s.Agent().State().Transcript {
		if o, ok := it.(*openresponses.FunctionCallOutput); ok {
			out = append(out, o.Output.Text)
		}
	}
	return out
}

func toolNames(s *agent.Session) []string {
	var names []string
	for _, tl := range s.Tools() {
		names = append(names, tl.Name)
	}
	return names
}

func requestTools(req openresponses.Request) string {
	var names []string
	for _, tl := range req.Tools {
		if f, ok := tl.(*openresponses.FunctionTool); ok {
			names = append(names, f.Name)
		}
	}
	return strings.Join(names, ",")
}

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

func runTask(t *testing.T, childCalls [][2]string, rules policy.Rules, fallback string, approve func(*openresponses.FunctionCall, string) bool) (agent.Options, *taskModels, *agent.Session) {
	t.Helper()
	model := &taskModels{
		parent: scripted{calls: [][2]string{{"task", `{"input":"write hello.txt saying hi"}`}}},
		child:  scripted{calls: childCalls},
	}
	o := options(t, model, &agents.Options{Model: "flash"})
	o.Model = "pro"
	o.Instructions = "Use tabs."
	o.Policy = &policy.Settings{Builtin: true, Fallback: fallback, User: rules}
	s, err := agent.New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := prompt(context.Background(), s, "go", approve); err != nil {
		t.Fatal(err)
	}
	return o, model, s
}

func TestATaskSubagentChangesTheProject(t *testing.T) {
	o, model, s := runTask(t, [][2]string{{"write", `{"path":"hello.txt","content":"hi\n"}`}}, policy.Rules{}, "allow", nil)
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
	if !strings.Contains(s.Agent().Config().Instructions, "let sub-agents do") {
		t.Error("the main agent's instructions lack the guide to its sub-agents")
	}
	if got := requestTools(req); got != "read,write,edit,glob,grep,ls,bash" {
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
	o, _, _ := runTask(t, [][2]string{{"write", `{"path":"hello.txt","content":"hi\n"}`}}, policy.Rules{Deny: []string{"write"}}, "allow", nil)
	if _, err := os.Stat(filepath.Join(o.Dir, "hello.txt")); err == nil {
		t.Error("a denied write ran in the task child")
	}

	// An ask goes to the user, named as the task sub-agent's.
	var asked []string
	o, _, _ = runTask(t, [][2]string{{"write", `{"path":"hello.txt","content":"hi\n"}`}}, policy.Rules{}, "ask", func(c *openresponses.FunctionCall, reason string) bool {
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
	o, model, _ := runTask(t, [][2]string{{"write", `{"path":"hello.txt","content":"hi\n"}`}}, policy.Rules{}, "ask", nil)
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
	o := options(t, &scripted{}, nil)
	s, err := agent.New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, n := range toolNames(s) {
		if n == "task" || n == "explore" {
			t.Errorf("%s offered without dax-agents", n)
		}
	}
}

// The sub-agent's session records a decision for each call the policy
// allowed, as the main agent's does.
func TestATaskRecordsItsPolicyDecisions(t *testing.T) {
	o, _, s := runTask(t, [][2]string{{"write", `{"path":"hello.txt","content":"hi\n"}`}}, policy.Rules{}, "allow", nil)
	parent := s.ID()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	sums, err := agent.List(context.Background(), o.Root, o.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, sum := range sums {
		if sum.Header.ParentSession != parent {
			continue
		}
		path, err := agent.Project(context.Background(), o.Root, sum.Header.ID, t.TempDir())
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
		o := options(t, model, &agents.Options{})
		s, err := agent.New(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := prompt(context.Background(), s, "go", nil); err != nil {
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

// A sub-agent's held call is a question to whoever holds the Turn's
// questions, not a permission; a refusal's note reaches the sub-agent,
// and with nobody holding the questions the call is refused with a
// reason the model can act on.
func TestASubagentsHeldCallIsAQuestion(t *testing.T) {
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
			o := options(t, model, &agents.Options{})
			o.Policy = &policy.Settings{Builtin: true, Fallback: "ask"}
			var asked []string
			rules := agent.Rules{Permit: func(*openresponses.FunctionCall, string) (bool, string) {
				t.Error("a permission was asked about a sub-agent's call, which is a question")
				return true, ""
			}}
			if tc.ask.err == nil {
				rules.Reply = func(q agent.Question) agent.Reply {
					asked = append(asked, q.Call.Name+": "+q.Text)
					return agent.Reply{Accept: tc.ask.allow, Note: tc.ask.note}
				}
			}
			s, err := agent.New(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if _, err := agent.Drive(context.Background(), s.Turn(), rules, openresponses.UserText("go")); err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.ask.err != nil && len(asked) != 0:
				// Nobody holds the questions: nothing is asked, and the
				// sub-agent is told to have the main agent make the call.
				t.Errorf("asked %q with nobody holding the questions", asked)
			case tc.ask.err == nil && (len(asked) != 1 || !strings.HasPrefix(asked[0], "write: the task sub-agent asks")):
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

// childModels routes each request by who asks it, the main agent, the
// explore child or the task child, and keeps the children's requests.
type childModels struct {
	parent        scripted
	mu            sync.Mutex
	explore, task []openresponses.Request
}

func (m *childModels) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	m.mu.Lock()
	switch {
	case strings.Contains(req.Instructions, "read-only explorer"):
		m.explore = append(m.explore, req)
		m.mu.Unlock()
		return (&scripted{}).CreateStream(ctx, req, sink)
	case strings.Contains(req.Instructions, "You are a sub-agent of"):
		m.task = append(m.task, req)
		m.mu.Unlock()
		return (&scripted{}).CreateStream(ctx, req, sink)
	}
	m.mu.Unlock()
	return m.parent.CreateStream(ctx, req, sink)
}

// late is an extension listed after dax-agents, with a tool that only
// looks and one that changes something.
func late() extension.Extension {
	tool := func(name string) agenttool.Tool {
		return agenttool.NewFunc(name, "A tool of a later extension.", json.RawMessage(`{"type":"object"}`),
			func(context.Context, agenttool.Call) (agenttool.Result, error) { return agenttool.Text(name), nil })
	}
	return extension.Extension{
		Name:     "late",
		Tools:    func(extension.ToolEnv) []agenttool.Tool { return []agenttool.Tool{tool("peek"), tool("poke")} },
		ReadOnly: []string{"peek"},
	}
}

// The sub-agents take every extension's tools, whatever order the
// extensions come in: explore the read-only ones, which its prompt
// names, and task all of them; a program named otherwise names itself
// to task.
func TestTheSubagentsTakeEveryExtensionsTools(t *testing.T) {
	model := &childModels{parent: scripted{calls: [][2]string{
		{"explore", `{"input":"where is main?"}`},
		{"task", `{"input":"poke it"}`},
	}}}
	o := options(t, model, &agents.Options{})
	o.Name, o.Version = "acme", "1.0"
	o.Extensions = append(o.Extensions, late())
	s, err := agent.New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := prompt(context.Background(), s, "go", nil); err != nil {
		t.Fatal(err)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if len(model.explore) == 0 || len(model.task) == 0 {
		t.Fatalf("explore asked %d times, task %d", len(model.explore), len(model.task))
	}
	ex, tk := model.explore[0], model.task[0]
	if got := requestTools(ex); got != "read,glob,grep,ls,bash,peek" {
		t.Errorf("explore's tools %s", got)
	}
	if !strings.Contains(ex.Instructions, "using the read, glob, grep, ls, bash and peek tools") {
		t.Errorf("explore's prompt does not name its tools:\n%s", ex.Instructions)
	}
	if got := requestTools(tk); got != "read,write,edit,glob,grep,ls,bash,peek,poke" {
		t.Errorf("task's tools %s", got)
	}
	if !strings.Contains(tk.Instructions, "You are a sub-agent of acme.") || !strings.Contains(tk.Instructions, "You are acme, a coding agent") {
		t.Errorf("task's prompt does not name the program:\n%s", tk.Instructions)
	}
}
