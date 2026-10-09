package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agenttool/mcpserver"
	"github.com/ChristopherDavenport/agentturn"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ChristopherDavenport/dax/ext/coding"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/internal/executor"
	"github.com/ChristopherDavenport/dax/policy"
	"github.com/ChristopherDavenport/dax/tool"
)

// executorRoot is set in the environment of the test binary run as an
// executor (TestMain).
const executorRoot = "DAX_TEST_EXECUTOR_ROOT"

// serveExecutor is `dax execute` over dir with dax-coding, on standard
// input and output: what the test binary runs as when executorRoot is
// set.
func serveExecutor(dir string) error {
	ws, err := workspace.NewLocal(dir, tool.DefaultEnv(nil))
	if err != nil {
		return err
	}
	defer ws.Close()
	srv, closeTools, err := executor.NewServer(executor.ServeOptions{Name: "dax", Version: "child", Descriptor: workspace.Descriptor{Kind: workspace.KindContainer, Ref: "box", Root: ws.Root()}},
		[]extension.Extension{coding.New(0)}, extension.ToolEnv{Workspace: ws, Files: tool.NewFiles(ws)})
	if err != nil {
		return err
	}
	defer closeTools()
	return srv.Run(context.Background(), &mcp.StdioTransport{})
}

// acme is a third party's extension as both sides have it: a tool
// that asks the user, one whose replay claim is safe, and one whose
// facts claim cannot be made.
func acme() extension.Extension {
	return extension.Extension{
		Name: "acme",
		Tools: func(extension.ToolEnv) []agenttool.Tool {
			return []agenttool.Tool{
				agenttool.NewFunc("ask", "Ask the user.", json.RawMessage(`{"type":"object"}`), func(ctx context.Context, _ agenttool.Call) (agenttool.Result, error) {
					e, ok := agenttool.ElicitorFrom(ctx)
					if !ok {
						return agenttool.Text("nobody to ask"), nil
					}
					a, err := e(ctx, agenttool.Elicitation{Message: "Ship it?", Schema: json.RawMessage(`{"type":"object"}`)})
					if err != nil {
						return agenttool.Result{}, err
					}
					return agenttool.Text("answer " + string(a.Action)), nil
				}),
				agenttool.NewFunc("status", "Status.", json.RawMessage(`{"type":"object"}`), func(context.Context, agenttool.Call) (agenttool.Result, error) {
					return agenttool.Text("green"), nil
				}, agenttool.WithReplay(func(context.Context, json.RawMessage) agenttool.Replay { return agenttool.ReplaySafe })),
				agenttool.NewFunc("broken", "Claims what it cannot.", json.RawMessage(`{"type":"object"}`), func(context.Context, agenttool.Call) (agenttool.Result, error) {
					return agenttool.Text("broken ran"), nil
				}, agenttool.WithFacts(func(context.Context, json.RawMessage) (agenttool.Facts, error) {
					return agenttool.Facts{}, errors.New("cannot read the deploy target")
				})),
			}
		},
		ReadOnly: []string{"status"},
		Policy:   policy.Rules{Allow: []string{"status", "broken", "ask"}},
	}
}

// remoteBox is an executor served in this process over a fresh
// directory, with what it was sent counted.
type remoteBox struct {
	dir string
	srv *mcp.Server

	mu    sync.Mutex
	facts []string // the arguments of each facts request's calls
	calls []string // the arguments of each tools/call
	// beforeCall, when set, runs as a call reaches the executor, after
	// every decision about it.
	beforeCall func()
	// factsErr, when set, fails every facts request.
	factsErr error
}

func newRemoteBox(t *testing.T) *remoteBox {
	t.Helper()
	dir := t.TempDir()
	ws, err := workspace.NewLocal(dir, tool.DefaultEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	srv, closeTools, err := executor.NewServer(executor.ServeOptions{Name: "dax", Version: "box", Descriptor: workspace.Descriptor{Kind: workspace.KindContainer, Ref: "box", Root: "/work"}},
		[]extension.Extension{coding.New(0), acme()}, extension.ToolEnv{Workspace: ws, Files: tool.NewFiles(ws)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTools() })
	b := &remoteBox{dir: dir, srv: srv}
	srv.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			raw, _ := json.Marshal(req.GetParams())
			switch method {
			case mcpserver.FactsMethod:
				var p struct {
					Calls []struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"calls"`
				}
				json.Unmarshal(raw, &p)
				b.mu.Lock()
				for _, c := range p.Calls {
					b.facts = append(b.facts, c.Name+" "+string(c.Arguments))
				}
				err := b.factsErr
				b.mu.Unlock()
				if err != nil {
					return nil, err
				}
			case "tools/call":
				var p struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				}
				json.Unmarshal(raw, &p)
				b.mu.Lock()
				b.calls = append(b.calls, p.Name+" "+string(p.Arguments))
				before := b.beforeCall
				b.mu.Unlock()
				if before != nil {
					before()
				}
			}
			return next(ctx, method, req)
		}
	})
	return b
}

// dial connects a session's Executor to the box in memory; the box's
// end is returned, so a test can hang up.
func (b *remoteBox) dial(t *testing.T) (*Executor, *mcp.ServerSession) {
	t.Helper()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := b.srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	ex, err := connectExecutor(ctx, ct, "dax", "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ex.Close() })
	return ex, ss
}

func (b *remoteBox) sent() (facts, calls []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.facts), slices.Clone(b.calls)
}

// instructed records each request's instructions.
type instructed struct {
	openresponses.Streamer
	mu   sync.Mutex
	seen []string
}

func (m *instructed) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	m.mu.Lock()
	m.seen = append(m.seen, req.Instructions)
	m.mu.Unlock()
	return m.Streamer.CreateStream(ctx, req, sink)
}

// remoteOptions is a session in a fresh project whose tools run in the
// box: dax-coding, dax-skills, dax-memory, dax-agents and acme, the
// project's own AGENTS.md in its directory here. No extension's Tools
// may be built in this process: the tools, their facts and their
// stamps are the executor's.
func remoteOptions(t *testing.T, model openresponses.Streamer, ex *Executor) Options {
	o := withAgents(options(t, model), "")
	o.Extensions = append(o.Extensions, acme())
	for i, e := range o.Extensions {
		if e.Tools != nil {
			e.Tools = func(extension.ToolEnv) []agenttool.Tool {
				t.Errorf("%s's Tools were built in the session's process", e.Name)
				return nil
			}
			o.Extensions[i] = e
		}
	}
	o.Executor = ex
	o.Policy = confirmPolicy(t)
	return o
}

// A session with an executor runs every extension's tools there: the
// model is offered them in the executor's order, a read the policy
// allows is decided on the executor's facts and runs there with its
// stamp, the main agent's and the explore sub-agent's alike, the
// record, the prompt and explore's prompt name the executor's
// workspace, and this directory's project files are not read.
func TestASessionRunsItsToolsInTheExecutor(t *testing.T) {
	ctx := context.Background()
	box := newRemoteBox(t)
	write(t, filepath.Join(box.dir, "notes.txt"), "remote notes\n")
	ex, _ := box.dial(t)
	read := [2]string{"read", `{"path":"notes.txt"}`}
	model := &instructed{Streamer: &twoModels{
		parent: scripted{calls: [][2]string{read, {"explore", `{"input":"read the notes"}`}}},
		child:  scripted{calls: [][2]string{read}},
	}}
	o := remoteOptions(t, model, ex)
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range s.Tools() {
		if tl.Source == "WithTools" {
			names = append(names, tl.Name)
		}
	}
	if want := []string{"read", "write", "edit", "glob", "grep", "ls", "bash", "ask", "status", "broken"}; !slices.Equal(names, want) {
		t.Errorf("tools %v, want %v", names, want)
	}
	omitted := ""
	for _, om := range s.Omitted() {
		omitted += om.String() + "\n"
	}
	if !strings.Contains(omitted, "/work") || !strings.Contains(omitted, "not read with an executor yet") {
		t.Errorf("omitted:\n%s", omitted)
	}
	var asked []string
	if _, err := promptOn(ctx, s, "read the notes", func(c *openresponses.FunctionCall, _ string) bool {
		asked = append(asked, c.Name)
		return false
	}); err != nil {
		t.Fatal(err)
	}
	outs := outputs(s)
	data := projected(t, o, s)

	if len(asked) != 0 {
		t.Errorf("asked about %v", asked)
	}
	if len(outs) == 0 || !strings.Contains(outs[0], "remote notes") {
		t.Errorf("outputs %q", outs)
	}
	facts, calls := box.sent()
	if len(calls) != 2 {
		t.Fatalf("the executor ran %v, want the main agent's read and the sub-agent's", calls)
	}
	for _, c := range calls {
		if !strings.HasPrefix(c, "read ") || !strings.Contains(c, "dax_stamp") {
			t.Errorf("the executor ran %s, want read with its stamp", c)
		}
	}
	// One reading of the model's arguments per decision. The engine
	// decides the stamped rewrite again unpinned, which is one more
	// reading each: the known cost until facts are batched.
	own, stamped := 0, 0
	for _, f := range facts {
		switch {
		case f == "read "+read[1]:
			own++
		case strings.HasPrefix(f, "read ") && strings.Contains(f, "dax_stamp"):
			stamped++
		default:
			t.Errorf("a facts request for %s", f)
		}
	}
	if own != 2 {
		t.Errorf("%d readings of the model's arguments, want one per decision (2): %v", own, facts)
	}
	if stamped != 2 {
		t.Errorf("%d readings of the stamped rewrite, want the known one per decision (2): %v", stamped, facts)
	}
	for _, w := range []string{`"kind":"container"`, `"ref":"box"`, `"cwd":"/work"`} {
		if !strings.Contains(string(data), w) {
			t.Errorf("the record lacks %s", w)
		}
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	main, explore := false, false
	for _, ins := range model.seen {
		if strings.Contains(ins, "Run go test before saying done") {
			t.Error("the prompt carries this directory's AGENTS.md")
		}
		if strings.Contains(ins, o.Dir) {
			t.Errorf("a prompt names this directory %s", o.Dir)
		}
		if strings.Contains(ins, "read-only explorer working in /work.") {
			explore = true
		} else if strings.Contains(ins, "/work") {
			main = true
		}
	}
	if !main || !explore {
		t.Errorf("the executor's root in the main prompt %v, in explore's %v", main, explore)
	}
}

// A file swapped for a link after the decision and before the call is
// caught where the file is: the executor checks the stamp and refuses.
func TestASwapBeforeTheCallIsRefusedByTheExecutor(t *testing.T) {
	ctx := context.Background()
	box := newRemoteBox(t)
	write(t, filepath.Join(box.dir, "notes.txt"), "notes\n")
	write(t, filepath.Join(box.dir, ".env"), "SECRET=1\n")
	box.beforeCall = func() {
		os.Remove(filepath.Join(box.dir, "notes.txt"))
		os.Symlink(".env", filepath.Join(box.dir, "notes.txt"))
	}
	ex, _ := box.dial(t)
	o := remoteOptions(t, &scripted{calls: [][2]string{{"read", `{"path":"notes.txt"}`}}}, ex)
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := promptOn(ctx, s, "read", func(*openresponses.FunctionCall, string) bool { return false }); err != nil {
		t.Fatal(err)
	}
	outs := outputs(s)
	if len(outs) != 1 || !strings.Contains(outs[0], "ask again") || strings.Contains(outs[0], "SECRET") {
		t.Errorf("outputs %q", outs)
	}
}

// A running command's output arrives as progress, a tool's question
// reaches whoever holds the Turn and its answer the tool, and the
// replay claim is the executor's.
func TestProgressQuestionsAndReplayCrossFromTheExecutor(t *testing.T) {
	ctx := context.Background()
	box := newRemoteBox(t)
	ex, _ := box.dial(t)
	model := &scripted{calls: [][2]string{{"bash", `{"command":"echo hi"}`}, {"ask", `{}`}}}
	o := remoteOptions(t, model, ex)
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var mu sync.Mutex
	var updates []string
	defer s.Turn().Subscribe(func(_ context.Context, e agentturn.Event) error {
		if u, ok := e.(*agentturn.ToolUpdate); ok {
			mu.Lock()
			updates = append(updates, u.Partial.Output.String())
			mu.Unlock()
		}
		return nil
	})()
	var questions []string
	rules := Rules{
		Permit: func(*openresponses.FunctionCall, string) (bool, string) { return true, "" },
		Reply: func(q Question) Reply {
			questions = append(questions, q.Text)
			return Reply{Accept: q.Call == nil}
		},
	}
	if _, err := Drive(ctx, s.Turn(), rules, openresponses.UserText("go")); err != nil {
		t.Fatal(err)
	}
	outs := outputs(s)
	if len(outs) != 2 || !strings.Contains(outs[0], "hi") || !strings.Contains(outs[1], "answer accept") {
		t.Errorf("outputs %q", outs)
	}
	mu.Lock()
	if !strings.Contains(strings.Join(updates, ""), "hi") {
		t.Errorf("updates %q", updates)
	}
	mu.Unlock()
	if len(questions) != 1 || !strings.Contains(questions[0], "Ship it?") {
		t.Errorf("questions %q", questions)
	}
	if got := s.x.Replay(ctx, executor.Call{Tool: "status", Call: agenttool.Call{Args: json.RawMessage(`{}`)}}); got != agenttool.ReplaySafe {
		t.Errorf("status replay %v", got)
	}
	if got := s.x.Replay(ctx, executor.Call{Tool: "read", Call: agenttool.Call{Args: json.RawMessage(`{"path":"x"}`)}}); got != agenttool.ReplayUnknown {
		t.Errorf("read replay %v", got)
	}
}

// When the executor cannot say what a call would touch, because its
// claim fails, it refuses the facts request, or it is gone, the call is
// blocked, the main agent's and a sub-agent's alike, however the policy
// would have decided it, and nothing runs.
func TestAnExecutorThatCannotAnswerBlocks(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		setup func(b *remoteBox, ss *mcp.ServerSession)
		call  [2]string
	}{
		{"the claim fails", func(*remoteBox, *mcp.ServerSession) {}, [2]string{"broken", `{}`}},
		{"the facts request fails", func(b *remoteBox, _ *mcp.ServerSession) { b.factsErr = errors.New("busy") }, [2]string{"read", `{"path":"notes.txt"}`}},
		{"the executor is gone", func(_ *remoteBox, ss *mcp.ServerSession) { ss.Close() }, [2]string{"read", `{"path":"notes.txt"}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := newRemoteBox(t)
			write(t, filepath.Join(box.dir, "notes.txt"), "notes\n")
			ex, ss := box.dial(t)
			model := &twoModels{
				parent: scripted{calls: [][2]string{tc.call, {"explore", `{"input":"look"}`}}},
				child:  scripted{calls: [][2]string{tc.call}},
			}
			o := remoteOptions(t, model, ex)
			s, err := New(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			tc.setup(box, ss)
			runCtx, cancel := context.WithTimeout(ctx, time.Minute)
			defer cancel()
			var asked []string
			promptOn(runCtx, s, "go", func(c *openresponses.FunctionCall, _ string) bool {
				asked = append(asked, c.Name)
				return true
			})
			if _, calls := box.sent(); len(calls) != 0 {
				t.Errorf("the executor ran %v", calls)
			}
			// Blocked, not asked, not run: the main agent's call and the
			// sub-agent's.
			outs := outputs(s)
			if len(outs) == 0 || !strings.Contains(outs[0], "could not be evaluated") {
				t.Errorf("the main agent's call: %q", outs)
			}
			model.mu.Lock()
			if len(model.seen) != 1 {
				t.Errorf("the sub-agent saw %v, want its one call's output", model.seen)
			}
			for _, o := range model.seen {
				if !strings.Contains(o, "could not be evaluated") {
					t.Errorf("the sub-agent's call: %q", o)
				}
			}
			model.mu.Unlock()
			for _, o := range outs {
				if strings.Contains(o, "notes") || strings.Contains(o, "broken ran") {
					t.Errorf("a call ran: %q", o)
				}
			}
			if slices.Contains(asked, tc.call[0]) {
				t.Errorf("asked about %s, which should have been blocked", tc.call[0])
			}
		})
	}
}

// What a session with an executor refuses: a workspace beside it, an
// MCP server (at start or added), an extension with tools the executor
// does not run, and a matcher that would give a served tool's subjects
// from this machine.
func TestASessionWithAnExecutorRefuses(t *testing.T) {
	ctx := context.Background()
	box := newRemoteBox(t)
	for _, tc := range []struct {
		name  string
		edit  func(o *Options)
		wants string
	}{
		{"a workspace", func(o *Options) {
			ws, err := workspace.NewLocal(t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ws.Close() })
			o.Workspace = ws
		}, "both an executor and a workspace"},
		{"an MCP server", func(o *Options) {
			o.MCP = []MCPServer{{Name: "fs", Command: os.Args[0]}}
		}, "MCP servers cannot run with an executor"},
		{"an extension the executor does not run", func(o *Options) {
			o.Extensions = append(o.Extensions, extension.Extension{Name: "other", Tools: func(extension.ToolEnv) []agenttool.Tool { return nil }})
		}, "the executor (dax box) does not run other"},
		{"a matcher giving a served tool's subjects", func(o *Options) {
			for i, e := range o.Extensions {
				if e.Name == "acme" {
					e.Matchers = extension.FixedMatchers(map[string]agentpolicy.ToolMatcher{"status": {
						Match:    agentpolicy.GlobMatcher("x"),
						Subjects: func(context.Context, json.RawMessage) ([]agentpolicy.Subject, error) { return nil, nil },
					}})
					o.Extensions[i] = e
				}
			}
		}, `the matcher for "status" gives its subjects`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ex, _ := box.dial(t)
			o := remoteOptions(t, &scripted{}, ex)
			tc.edit(&o)
			s, err := New(ctx, o)
			if err == nil {
				s.Close()
				t.Fatal("New succeeded")
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("err = %v, want %q", err, tc.wants)
			}
		})
	}
	ex, _ := box.dial(t)
	s, err := New(ctx, remoteOptions(t, &scripted{}, ex))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.AddMCP(ctx, "fs", os.Args[0]); err == nil || !strings.Contains(err.Error(), "MCP servers cannot run with an executor") {
		t.Errorf("AddMCP: %v", err)
	}
	// The session leaves the caller's executor open.
	s.Close()
	if _, err := ex.r.Tools(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.r.Facts(ctx, executor.Call{Tool: "read", Call: agenttool.Call{Args: json.RawMessage(`{"path":"x"}`)}}); err != nil {
		t.Errorf("the executor was closed with the session: %v", err)
	}
}

// DialExecutor starts the executor with this machine's environment
// less its credentials: the model's key, by the variable it came from
// though its name does not look like a credential's, never reaches the
// executor or what it runs. A program that is not an executor is
// refused.
func TestDialExecutorKeepsTheKeyHere(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	t.Setenv(executorRoot, t.TempDir())
	t.Setenv("DAXMODELVAR", "sk-model")
	t.Setenv("MY_TOKEN", "tok-leak")
	t.Setenv("PLAINVAR", "visible")
	ex, err := DialExecutor(ctx, os.Args[0], ExecutorOptions{Name: "dax", Version: "test", KeyEnv: "DAXMODELVAR"})
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()
	if name, version := ex.Server(); name != "dax" || version != "child" {
		t.Errorf("server %s %s", name, version)
	}
	if d := ex.Workspace().Descriptor(); d.Kind != workspace.KindContainer || d.Ref != "box" {
		t.Errorf("descriptor %+v", d)
	}
	res, err := ex.r.Call(ctx, executor.Call{Tool: "bash", Call: agenttool.Call{ID: "b", Args: json.RawMessage(`{"command":"echo key=[$DAXMODELVAR] token=[$MY_TOKEN] plain=[$PLAINVAR]"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Output.String(); !strings.Contains(got, "key=[] token=[] plain=[visible]") {
		t.Errorf("the executor's command saw %q", got)
	}

	t.Setenv(executorRoot, "")
	t.Setenv("DAX_TEST_MCP_SERVER", "1")
	if ex, err := DialExecutor(ctx, os.Args[0], ExecutorOptions{}); err == nil || !strings.Contains(err.Error(), "not a dax executor") {
		if ex != nil {
			ex.Close()
		}
		t.Errorf("a plain MCP server: %v", err)
	}
	if _, err := DialExecutor(ctx, filepath.Join(t.TempDir(), "nothing"), ExecutorOptions{}); err == nil {
		t.Error("a command that does not exist connected")
	}
	if _, err := DialExecutor(ctx, "  ", ExecutorOptions{}); err == nil {
		t.Error("an empty command connected")
	}
}

// The view of the executor's workspace acts on nothing.
func TestTheExecutorsViewActsOnNothing(t *testing.T) {
	ctx := context.Background()
	v := &executorView{d: workspace.Descriptor{Kind: workspace.KindRemote, Root: "/srv"}}
	if v.Root() != "/srv" {
		t.Errorf("root %s", v.Root())
	}
	if err := v.WriteFile(ctx, "x", nil, 0o644); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("WriteFile: %v", err)
	}
	if err := v.Remove(ctx, "x"); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Remove: %v", err)
	}
	if _, err := v.Exec(ctx, workspace.Command{Args: []string{"true"}}); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Exec: %v", err)
	}
	if _, ok := any(v).(workspace.Starter); ok {
		t.Error("the view is a Starter")
	}
	if _, err := v.FS().Open("AGENTS.md"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("open: %v", err)
	}
	if entries, err := fs.ReadDir(v.FS(), "."); err != nil || len(entries) != 0 {
		t.Errorf("readdir: %v %v", entries, err)
	}
}
