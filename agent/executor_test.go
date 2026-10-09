package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/ext/agents"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/internal/executor"
	"github.com/ChristopherDavenport/dax/policy"
	"github.com/ChristopherDavenport/dax/workspace"
)

// elsewhere is an executor that is not this process, as the session
// sees one: it lists acme's deploy, which claims its facts (a call of
// {"target": T} deploys to T, and runs as the plan approved-T), and
// records what it was asked.
type elsewhere struct {
	ext string // the extension its tool names

	mu     sync.Mutex
	facts  []executor.Call
	calls  []executor.Call
	closed int
}

func (x *elsewhere) Tools(context.Context) ([]executor.Tool, error) {
	return []executor.Tool{{
		Extension:  x.ext,
		Definition: openresponses.NewFunctionTool("deploy", "Deploy, elsewhere.", json.RawMessage(`{"type":"object","properties":{"target":{"type":"string"}}}`)),
		ReadOnly:   true,
		Factual:    true,
	}}, nil
}

func (x *elsewhere) Facts(_ context.Context, c executor.Call) (agenttool.Facts, error) {
	x.mu.Lock()
	x.facts = append(x.facts, c)
	x.mu.Unlock()
	var in struct{ Target, Plan string }
	if err := json.Unmarshal(c.Args, &in); err != nil {
		return agenttool.Facts{}, err
	}
	env := in.Target
	var rewrite json.RawMessage
	if in.Plan != "" {
		env = strings.TrimPrefix(in.Plan, "approved-")
	} else {
		rewrite, _ = json.Marshal(map[string]string{"plan": "approved-" + env})
	}
	subject, _ := json.Marshal(map[string]string{"env": env})
	return agenttool.Facts{Calls: []agenttool.FactCall{{Args: subject}}, Rewrite: rewrite}, nil
}

func (x *elsewhere) Replay(context.Context, executor.Call) agenttool.Replay {
	return agenttool.ReplayUnknown
}

func (x *elsewhere) Call(_ context.Context, c executor.Call) (agenttool.Result, error) {
	x.mu.Lock()
	x.calls = append(x.calls, c)
	x.mu.Unlock()
	return agenttool.Text("deployed elsewhere"), nil
}

func (x *elsewhere) Descriptor() workspace.Descriptor {
	return workspace.Descriptor{Kind: workspace.KindRemote, Ref: "runtime-7", Root: "/srv/app"}
}

func (x *elsewhere) Close() error {
	x.mu.Lock()
	x.closed++
	x.mu.Unlock()
	return nil
}

// tooled records the function tools each request offered.
type tooled struct {
	openresponses.Streamer
	mu    sync.Mutex
	tools []string
}

func (m *tooled) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	m.mu.Lock()
	for _, t := range req.Tools {
		if f, ok := t.(*openresponses.FunctionTool); ok {
			b, _ := json.Marshal(f)
			m.tools = append(m.tools, string(b))
		}
	}
	m.mu.Unlock()
	return m.Streamer.CreateStream(ctx, req, sink)
}

// The session runs the extensions' tools through its executor, wherever
// that is: the model is offered the executor's definitions, a call the
// main agent or a sub-agent makes runs there with the rewrite its facts
// asked for, its facts are read there once per call, the record names
// the executor's workspace, and the session closes it once. The
// extension's own Tools are never built.
func TestTheSessionRunsTheExtensionsToolsThroughItsExecutor(t *testing.T) {
	ctx := context.Background()
	x := &elsewhere{ext: "acme"}
	call := [2]string{"deploy", `{"target":"staging"}`}
	model := &tooled{Streamer: &twoModels{
		parent: scripted{calls: [][2]string{call, {"explore", `{"input":"ship it too"}`}}},
		child:  scripted{calls: [][2]string{call}},
	}}
	o := options(t, model)
	o.executor = x
	o.Policy = confirmPolicy(t)
	o.Extensions = []extension.Extension{
		{
			Name: "acme",
			Tools: func(extension.ToolEnv) []agenttool.Tool {
				t.Error("acme's Tools were built in this process")
				return nil
			},
			Matchers: extension.FixedMatchers(map[string]agentpolicy.ToolMatcher{"deploy": {Match: agentpolicy.GlobMatcher("env")}}),
			Policy:   policy.Rules{Allow: []string{"deploy(staging)"}},
		},
		agents.New(agents.Options{}),
	}
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	var asked []string
	if _, err := promptOn(ctx, s, "ship", func(c *openresponses.FunctionCall, _ string) bool {
		asked = append(asked, c.Name)
		return false
	}); err != nil {
		t.Fatal(err)
	}
	data := projected(t, o, s)

	x.mu.Lock()
	defer x.mu.Unlock()
	if len(asked) != 0 {
		t.Errorf("asked about %v", asked)
	}
	if len(x.calls) != 2 {
		t.Fatalf("the executor ran %d calls, want the main agent's and the sub-agent's", len(x.calls))
	}
	ids := map[string]bool{}
	for _, c := range x.calls {
		if c.Tool != "deploy" || string(c.Args) != `{"plan":"approved-staging"}` || c.ID == "" {
			t.Errorf("the executor ran %+v, want deploy with the rewrite", c)
		}
		ids[c.ID] = true
	}
	// One reading of the model's arguments per call, carrying its ID;
	// the rewrite's arguments are another call, read afresh.
	raw := map[string]int{}
	for _, f := range x.facts {
		if string(f.Args) == call[1] {
			raw[f.ID]++
		}
	}
	if len(raw) != 2 {
		t.Errorf("readings of the model's arguments by call ID: %v, want one for each of %v", raw, ids)
	}
	for id, n := range raw {
		if !ids[id] || n != 1 {
			t.Errorf("call %q: %d readings of the model's arguments, want 1", id, n)
		}
	}
	if x.closed != 1 {
		t.Errorf("closed %d times, want once", x.closed)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	want := `{"type":"function","name":"deploy","description":"Deploy, elsewhere.","parameters":{"type":"object","properties":{"target":{"type":"string"}}}}`
	found := false
	for _, tl := range model.tools {
		if strings.Contains(tl, `"name":"deploy"`) {
			found = true
			if tl != want {
				t.Errorf("the request offered %s, want %s", tl, want)
			}
		}
	}
	if !found {
		t.Errorf("no request offered deploy: %v", model.tools)
	}
	for _, w := range []string{`"kind":"remote"`, `"ref":"runtime-7"`, `"cwd":"/srv/app"`} {
		if !strings.Contains(string(data), w) {
			t.Errorf("the record lacks %s", w)
		}
	}
}

// A tool an executor says is of an extension the session does not have
// fails New, and the executor is closed.
func TestAToolOfNoExtensionInTheSessionFailsNew(t *testing.T) {
	x := &elsewhere{ext: "nobody"}
	o := options(t, &scripted{})
	o.executor = x
	o.Extensions = []extension.Extension{{Name: "acme"}}
	if s, err := New(context.Background(), o); err == nil || !strings.Contains(err.Error(), `names extension "nobody"`) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("err = %v", err)
	}
	if x.closed != 1 {
		t.Errorf("closed %d times, want once", x.closed)
	}
}
