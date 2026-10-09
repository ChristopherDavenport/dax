package executor

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ChristopherDavenport/agenttool"

	"github.com/ChristopherDavenport/dax/ext/coding"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/tool"
	"github.com/ChristopherDavenport/dax/workspace"
)

// toolEnv is a local workspace in a fresh directory, and the tools'
// view of it.
func toolEnv(t *testing.T) extension.ToolEnv {
	t.Helper()
	ws, err := workspace.NewLocal(t.TempDir(), tool.DefaultEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	return extension.ToolEnv{Workspace: ws, Files: tool.NewFiles(ws)}
}

// plain is a tool that claims nothing.
func plain(name string, opts ...agenttool.Option) agenttool.Tool {
	return agenttool.NewFunc(name, "A plain tool.", json.RawMessage(`{"type":"object"}`),
		func(_ context.Context, c agenttool.Call) (agenttool.Result, error) {
			return agenttool.Text(name + " ran " + string(c.Args)), nil
		}, opts...)
}

// bare is a third party's tool written without agenttool's
// constructors: it implements Tool and nothing else.
type bare struct{}

func (bare) Name() string                { return "bare" }
func (bare) Description() string         { return "Bare." }
func (bare) Parameters() json.RawMessage { return nil }
func (bare) Execute(context.Context, agenttool.Call) (agenttool.Result, error) {
	return agenttool.Text("bare ran"), nil
}

// keyed is a third party's tool that claims replay, as a type of its
// own: safe for a dry run, keyed otherwise.
type keyed struct{ bare }

func (keyed) Name() string { return "keyed" }
func (keyed) Replay(_ context.Context, args json.RawMessage) agenttool.Replay {
	if strings.Contains(string(args), "dry") {
		return agenttool.ReplaySafe
	}
	return agenttool.ReplayKeyed
}

// acme is a third party's extension with one tool of each kind.
func acme(closed *atomic.Int32) extension.Extension {
	return extension.Extension{
		Name: "acme",
		Tools: func(extension.ToolEnv) []agenttool.Tool {
			return []agenttool.Tool{
				plain("deploy", agenttool.WithResource("acme:deploys"), agenttool.WithStrict(),
					agenttool.WithAnnotations(agenttool.Annotations{Title: "Deploy", Destructive: true}),
					agenttool.WithCloser(func() error { closed.Add(1); return nil })),
				plain("status", agenttool.WithReplay(func(context.Context, json.RawMessage) agenttool.Replay { return agenttool.ReplaySafe })),
				bare{},
				keyed{},
			}
		},
		ReadOnly: []string{"status"},
	}
}

func newInProcess(t *testing.T, exts ...extension.Extension) (Executor, extension.ToolEnv) {
	t.Helper()
	env := toolEnv(t)
	x, err := InProcess(exts, env)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { x.Close() })
	return x, env
}

// The in-process executor lists every extension's tools in order, with
// what the session and the scheduler read of each.
func TestTheToolsComeInTheExtensionsOrder(t *testing.T) {
	var closed atomic.Int32
	x, env := newInProcess(t, coding.New(0), acme(&closed))
	got, err := x.Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	inner := append(tool.Builtins(env.Files, 0), acme(&closed).Tools(env)...)
	if len(got) != len(inner) {
		t.Fatalf("%d tools, want %d", len(got), len(inner))
	}
	readOnly := []string{"read", "glob", "grep", "ls", "bash", "status"}
	for i, want := range inner {
		g := got[i]
		ext := "acme"
		if i < 7 {
			ext = coding.Name
		}
		if g.Name() != want.Name() || g.Extension != ext {
			t.Errorf("tool %d = %s of %s, want %s of %s", i, g.Name(), g.Extension, want.Name(), ext)
		}
		if !reflect.DeepEqual(g.Definition, agenttool.Definition(want)) {
			t.Errorf("%s: definition %+v, want %+v", g.Name(), g.Definition, agenttool.Definition(want))
		}
		_, replayable := want.(agenttool.Replayable)
		if g.ReadOnly != slices.Contains(readOnly, g.Name()) || g.Sequential != (g.Name() == "bash") ||
			g.Factual != (i < 7) || g.Replayable != replayable || g.Resource != agenttool.ResourceOf(want) ||
			g.Annotations != agenttool.AnnotationsOf(want) {
			t.Errorf("%s: %+v", g.Name(), g)
		}
	}
	if bareTool, keyedTool := got[len(got)-2], got[len(got)-1]; bareTool.Replayable || !keyedTool.Replayable {
		t.Errorf("bare replayable %v, keyed replayable %v", bareTool.Replayable, keyedTool.Replayable)
	}
	if x.Descriptor() != env.Workspace.Descriptor() {
		t.Errorf("descriptor %+v, want %+v", x.Descriptor(), env.Workspace.Descriptor())
	}
}

// Facts and Replay are the tool's own answers, and Call runs the tool
// with the call as given: its ID, arguments, key, progress, and what
// the context carries, the elicitor among it.
func TestTheExecutorAnswersAsTheToolDoes(t *testing.T) {
	ctx := context.Background()
	var closed atomic.Int32
	x, env := newInProcess(t, coding.New(0), acme(&closed))
	read := tool.Builtins(env.Files, 0)[0]
	for _, args := range []string{`{"path":"x.txt"}`, `{"path":"../out"}`, `{"Path":"x"}`} {
		want, _, wantErr := agenttool.FactsOf(ctx, read, json.RawMessage(args))
		got, err := x.Facts(ctx, Call{Tool: "read", Call: agenttool.Call{Args: json.RawMessage(args)}})
		if !reflect.DeepEqual(got, want) || (err == nil) != (wantErr == nil) {
			t.Errorf("Facts(read %s) = %+v, %v; want %+v, %v", args, got, err, want, wantErr)
		}
	}
	if f, err := x.Facts(ctx, Call{Tool: "status", Call: agenttool.Call{Args: json.RawMessage(`{}`)}}); err != nil || !reflect.DeepEqual(f, agenttool.Facts{}) {
		t.Errorf("Facts of a tool with no claim = %+v, %v", f, err)
	}
	for name, want := range map[string]agenttool.Replay{"status": agenttool.ReplaySafe, "deploy": agenttool.ReplayUnknown, "bare": agenttool.ReplayUnknown, "keyed": agenttool.ReplayKeyed} {
		if got := x.Replay(ctx, Call{Tool: name, Call: agenttool.Call{Args: json.RawMessage(`{}`)}}); got != want {
			t.Errorf("Replay(%s) = %v, want %v", name, got, want)
		}
	}

	var mu sync.Mutex
	var progress []string
	args, _ := json.Marshal(map[string]string{"command": "echo hi"})
	res, err := x.Call(ctx, Call{Tool: "bash", Call: agenttool.Call{ID: "c1", Args: args, OnUpdate: func(r agenttool.Result) {
		mu.Lock()
		progress = append(progress, r.Output.Text)
		mu.Unlock()
	}}})
	if err != nil || !strings.Contains(res.Output.Text, "hi") {
		t.Fatalf("bash = %+v, %v", res, err)
	}
	mu.Lock()
	if len(progress) == 0 || !strings.Contains(progress[0], "hi") {
		t.Errorf("progress %q", progress)
	}
	mu.Unlock()

	var seen agenttool.Call
	var asked bool
	probe := extension.Extension{Name: "probe", Tools: func(extension.ToolEnv) []agenttool.Tool {
		return []agenttool.Tool{agenttool.NewFunc("probe", "Probe.", nil, func(ctx context.Context, c agenttool.Call) (agenttool.Result, error) {
			seen = c
			if e, ok := agenttool.ElicitorFrom(ctx); ok {
				_, err := e(ctx, agenttool.Elicitation{})
				asked = err == nil
			}
			return agenttool.Text("ok"), nil
		})}
	}}
	px, _ := newInProcess(t, probe)
	ectx := agenttool.ContextWithElicitor(ctx, func(context.Context, agenttool.Elicitation) (agenttool.Answer, error) {
		return agenttool.Answer{}, nil
	})
	updates := 0
	in := agenttool.Call{ID: "c9", Args: json.RawMessage(`{"a":1}`), IdempotencyKey: "k9", OnUpdate: func(agenttool.Result) { updates++ }}
	if _, err := px.Call(ectx, Call{Tool: "probe", Call: in}); err != nil {
		t.Fatal(err)
	}
	seen.OnUpdate(agenttool.Result{})
	if seen.ID != "c9" || string(seen.Args) != `{"a":1}` || seen.IdempotencyKey != "k9" || updates != 1 || !asked {
		t.Errorf("the tool saw %+v (updates %d, asked %v)", seen, updates, asked)
	}
	if _, err := px.Call(ctx, Call{Tool: "nope"}); err == nil {
		t.Error("a call of no tool ran")
	}
}

// Close closes each tool that holds something, once, however often it
// is called; a refused build closes what it built.
func TestCloseClosesEachToolOnce(t *testing.T) {
	var closed atomic.Int32
	x, err := InProcess([]extension.Extension{acme(&closed)}, toolEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	x.Close()
	x.Close()
	if n := closed.Load(); n != 1 {
		t.Errorf("closed %d times, want once", n)
	}
	closed.Store(0)
	broken := extension.Extension{Name: "broken", Tools: func(extension.ToolEnv) []agenttool.Tool { return []agenttool.Tool{nil} }}
	if _, err := InProcess([]extension.Extension{acme(&closed), broken}, toolEnv(t)); err == nil || err.Error() != "extension broken: a nil tool" {
		t.Errorf("err = %v", err)
	}
	if n := closed.Load(); n != 1 {
		t.Errorf("a refused build closed %d times, want once", n)
	}
}

// An adapter is its tool to everything agenttool reads of a tool: the
// definition a request carries, scheduling, annotations, strictness,
// the facts claim and the replay claim, a third party's own type's
// included. It owns nothing, so it is not a Closer.
func TestAnAdapterIsItsToolToAgenttool(t *testing.T) {
	ctx := context.Background()
	var closed atomic.Int32
	x, env := newInProcess(t, coding.New(0), acme(&closed))
	s, err := Bind(ctx, x)
	if err != nil {
		t.Fatal(err)
	}
	inner := append(tool.Builtins(env.Files, 0), acme(&closed).Tools(env)...)
	bound := s.Tools()
	if len(bound) != len(inner) {
		t.Fatalf("%d bound, want %d", len(bound), len(inner))
	}
	argSets := []string{`{}`, `{"dry":true}`, `{"path":"a"}`, `{"command":"ls"}`, `{"command":"git push"}`}
	for i, want := range inner {
		ad := bound[i].Adapter
		if !reflect.DeepEqual(agenttool.Definition(ad), agenttool.Definition(want)) {
			t.Errorf("%s: definition %+v, want %+v", want.Name(), agenttool.Definition(ad), agenttool.Definition(want))
		}
		if agenttool.IsSequential(ad) != agenttool.IsSequential(want) || agenttool.ResourceOf(ad) != agenttool.ResourceOf(want) ||
			agenttool.AnnotationsOf(ad) != agenttool.AnnotationsOf(want) || agenttool.IsStrict(ad) != agenttool.IsStrict(want) ||
			agenttool.IsFactual(ad) != agenttool.IsFactual(want) {
			t.Errorf("%s: the adapter differs from its tool", want.Name())
		}
		for _, args := range argSets {
			if g, w := agenttool.ReplayOf(ctx, ad, json.RawMessage(args)), agenttool.ReplayOf(ctx, want, json.RawMessage(args)); g != w {
				t.Errorf("%s %s: ReplayOf = %v, want %v", want.Name(), args, g, w)
			}
		}
		if _, ok := ad.(io.Closer); ok {
			t.Errorf("%s: the adapter is a Closer", want.Name())
		}
		if b, ok := s.ByName(want.Name()); !ok || b.Adapter != ad {
			t.Errorf("ByName(%s) = %v, %v", want.Name(), b.Name(), ok)
		}
	}
	keyed, _ := s.ByName("keyed")
	if got := agenttool.ReplayOf(ctx, keyed.Adapter, json.RawMessage(`{"dry":true}`)); got != agenttool.ReplaySafe {
		t.Errorf("keyed's replay claim through the adapter = %v", got)
	}
	// Running an adapter is running the tool.
	status, _ := s.ByName("status")
	if res, err := status.Adapter.Execute(ctx, agenttool.Call{Args: json.RawMessage(`{"x":1}`)}); err != nil || res.Output.Text != `status ran {"x":1}` {
		t.Errorf("status = %+v, %v", res, err)
	}
}
