package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agenttool/mcpclient"
	"github.com/ChristopherDavenport/agenttool/mcpserver"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ChristopherDavenport/dax/ext/coding"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/tool"
)

// served is a third party's extension as an executor serves it: a
// strict tool with a resource (bash is the sequential one), a read-only one whose
// replay claim is safe, and one that asks the user, listed out of the
// name order MCP lists tools in.
func served() extension.Extension {
	return extension.Extension{
		Name: "acme",
		Tools: func(extension.ToolEnv) []agenttool.Tool {
			return []agenttool.Tool{
				plain("status", agenttool.WithReplay(func(context.Context, json.RawMessage) agenttool.Replay { return agenttool.ReplaySafe })),
				plain("deploy", agenttool.WithResource("acme:deploys"), agenttool.WithStrict()),
				agenttool.NewFunc("ask", "Ask the user.", json.RawMessage(`{"type":"object"}`), func(ctx context.Context, _ agenttool.Call) (agenttool.Result, error) {
					e, ok := agenttool.ElicitorFrom(ctx)
					if !ok {
						return agenttool.Text("nobody to ask"), nil
					}
					a, err := e(ctx, agenttool.Elicitation{Message: "Which?", Schema: json.RawMessage(`{"type":"object","properties":{"pick":{"type":"string"}}}`)})
					if err != nil {
						return agenttool.Result{}, err
					}
					return agenttool.Text(string(a.Action) + " " + string(a.Content)), nil
				}),
			}
		},
		ReadOnly: []string{"status"},
	}
}

// quiet is an extension with no Tools, which the capability leaves out.
var quiet = extension.Extension{Name: "quiet"}

// executorServer is NewServer over a local workspace in a fresh
// directory, connected in memory; it returns the directory, the
// workspace and a count of the facts requests it was sent.
type executorServer struct {
	dir   string
	ws    workspace.Workspace
	srv   *sdk.Server
	facts *atomic.Int32
}

var testDescriptor = func(root string) workspace.Descriptor {
	return workspace.Descriptor{Kind: workspace.KindContainer, Ref: "box", Root: root}
}

func newServer(t *testing.T) executorServer {
	t.Helper()
	dir := t.TempDir()
	ws, err := workspace.NewLocal(dir, tool.DefaultEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	env := extension.ToolEnv{Workspace: ws, Files: tool.NewFiles(ws)}
	srv, closeTools, err := NewServer(ServeOptions{Name: "dax", Version: "test", Descriptor: testDescriptor(ws.Root())}, []extension.Extension{coding.New(0), quiet, served()}, env)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTools() })
	var facts atomic.Int32
	srv.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			if method == mcpserver.FactsMethod {
				facts.Add(1)
			}
			return next(ctx, method, req)
		}
	})
	return executorServer{dir: dir, ws: ws, srv: srv, facts: &facts}
}

// connect is a raw mcpclient over s, with opts.
func (s executorServer) connect(t *testing.T, opts ...mcpclient.Option) *mcpclient.Remote {
	t.Helper()
	ctx := context.Background()
	st, ct := sdk.NewInMemoryTransports()
	ss, err := s.srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	r, err := mcpclient.Connect(ctx, ct, append([]mcpclient.Option{mcpclient.WithNotificationGrace(0)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func remoteTool(t *testing.T, r *mcpclient.Remote, name string) agenttool.Tool {
	t.Helper()
	for _, tl := range r.Tools() {
		if tl.Name() == name {
			return tl
		}
	}
	t.Fatalf("no tool %s", name)
	return nil
}

// The capability says what MCP cannot: the descriptor, the extensions
// with Tools, and each tool in the order it runs in process with its
// extension, its extension's ReadOnly, whether it claims facts and
// whether it is strict. The listing's _meta carries sequential and
// resource; the server says the tools never change and that it answers
// the facts method.
func TestTheExecutorSaysWhatMCPCannot(t *testing.T) {
	s := newServer(t)
	r := s.connect(t)
	init := r.Session().InitializeResult()
	if init == nil || init.Capabilities == nil {
		t.Fatal("no capabilities")
	}
	caps := init.Capabilities
	if caps.Tools == nil || caps.Tools.ListChanged {
		t.Errorf("tools capability %+v, want listChanged false", caps.Tools)
	}
	if _, ok := caps.Experimental[mcpclient.FactsCapability]; !ok {
		t.Errorf("no %s in %v", mcpclient.FactsCapability, caps.Experimental)
	}
	raw, err := json.Marshal(caps.Experimental[CapabilityKey])
	if err != nil {
		t.Fatal(err)
	}
	var got capability
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}

	in, err := InProcess([]extension.Extension{coding.New(0), quiet, served()}, extension.ToolEnv{Workspace: s.ws, Files: tool.NewFiles(s.ws)})
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	tools, _ := in.Tools(context.Background())
	var want []capabilityTool
	for _, tl := range tools {
		want = append(want, capabilityTool{Name: tl.Name(), Extension: tl.Extension, ReadOnly: tl.ReadOnly, Facts: tl.Factual, Strict: tl.Name() == "deploy"})
	}
	d := testDescriptor(s.ws.Root())
	if got.Version != CapabilityVersion || got.Descriptor != (capabilityDescriptor{Kind: d.Kind, Ref: d.Ref, Root: d.Root}) ||
		!slices.Equal(got.Extensions, []string{coding.Name, "acme"}) || !reflect.DeepEqual(got.Tools, want) {
		t.Errorf("capability %s\nwant tools %+v", raw, want)
	}
	// The listing is sorted by name, so the order is the capability's.
	names := func(c []capabilityTool) []string {
		var n []string
		for _, x := range c {
			n = append(n, x.Name)
		}
		return n
	}
	if n := names(got.Tools); !slices.Equal(n, []string{"read", "write", "edit", "glob", "grep", "ls", "bash", "status", "deploy", "ask"}) {
		t.Errorf("order %v", n)
	}
	if status := got.Tools[7]; !status.ReadOnly || status.Facts || status.Extension != "acme" {
		t.Errorf("status %+v", status)
	}

	listed, err := r.Session().ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	metas := map[string]mcpclient.ToolMeta{}
	for _, tl := range listed.Tools {
		m, _ := mcpclient.ToolMetaOf(tl)
		metas[tl.Name] = m
	}
	if m := metas["deploy"]; m.Resource != "acme:deploys" || !m.Replay {
		t.Errorf("deploy's _meta %+v", m)
	}
	if m := metas["bash"]; !m.Sequential || !m.Facts {
		t.Errorf("bash's _meta %+v", m)
	}
	if m := metas["status"]; !m.Replay || m.Facts {
		t.Errorf("status's _meta %+v", m)
	}
}

// A served tool's facts are the in-process tool's: bash's claim for a
// command that reads one file and writes another crosses whole.
func TestTheFactsMethodAnswersAsTheToolDoesInProcess(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	os.WriteFile(filepath.Join(s.dir, ".env"), []byte("SECRET=1\n"), 0o644)
	os.Mkdir(filepath.Join(s.dir, "out"), 0o755)
	r := s.connect(t, mcpclient.WithClaims())
	args := json.RawMessage(`{"command":"cat .env > out/x"}`)
	got, err := r.Facts(ctx, mcpclient.FactsCall{Name: "bash", Args: args})
	if err != nil || got[0].Err != nil || !got[0].Claimed {
		t.Fatalf("Facts = %+v, %v", got, err)
	}
	local := tool.Bash(tool.NewFiles(s.ws))
	want, _, err := agenttool.FactsOf(ctx, local, args)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got[0].Facts.Calls, want.Calls) || string(got[0].Facts.Rewrite) != string(want.Rewrite) {
		t.Errorf("served facts %+v, in process %+v", got[0].Facts, want)
	}
	if len(want.Calls) < 2 {
		t.Errorf("the claim names %d calls; want the read of .env and the write of out/x", len(want.Calls))
	}
}

// The stamp is checked where the tool runs. A read of notes.txt is
// decided, its facts are stamped by the executor, notes.txt becomes a
// link to .env, and the call with the stamped arguments is refused by
// the executor. A bash plan likewise, once the line no longer analyses
// to it: notes.txt becomes a link out of the workspace. Unswapped, both
// run. A link to .env inside the workspace would not do for bash: its
// stamp signs the rendered plan alone, which such a swap leaves as it
// was, so the call runs, in process too. That gap is bash's, with a
// fix of its own, not the executor's.
func TestAStampIsCheckedInTheExecutor(t *testing.T) {
	ctx := context.Background()
	outside := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(outside, []byte("SECRET=2\n"), 0o644)
	for _, tc := range []struct {
		tool, args, ran, link string
	}{
		{"read", `{"path":"notes.txt"}`, "notes", ".env"},
		{"bash", `{"command":"cat notes.txt"}`, "notes", outside},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			setup := func(t *testing.T) (executorServer, agenttool.Tool, string) {
				s := newServer(t)
				os.WriteFile(filepath.Join(s.dir, "notes.txt"), []byte("notes\n"), 0o644)
				os.WriteFile(filepath.Join(s.dir, ".env"), []byte("SECRET=1\n"), 0o644)
				tl := remoteTool(t, s.connect(t, mcpclient.WithClaims()), tc.tool)
				if !agenttool.IsFactual(tl) {
					t.Fatalf("%s is not factual over the wire", tc.tool)
				}
				f, _, err := agenttool.FactsOf(ctx, tl, json.RawMessage(tc.args))
				if err != nil || !strings.Contains(string(f.Rewrite), "dax_stamp") {
					t.Fatalf("%s %s not stamped: %+v %v", tc.tool, tc.args, f, err)
				}
				return s, tl, string(f.Rewrite)
			}

			_, tl, stamped := setup(t)
			res, err := tl.Execute(ctx, agenttool.Call{ID: "c1", Args: json.RawMessage(stamped)})
			if err != nil || !strings.Contains(res.Output.String(), tc.ran) {
				t.Fatalf("unchanged: %+v, %v", res, err)
			}

			s, tl, stamped := setup(t)
			os.Remove(filepath.Join(s.dir, "notes.txt"))
			if err := os.Symlink(tc.link, filepath.Join(s.dir, "notes.txt")); err != nil {
				t.Fatal(err)
			}
			res, err = tl.Execute(ctx, agenttool.Call{ID: "c2", Args: json.RawMessage(stamped)})
			out := res.Output.String()
			if err != nil {
				out += err.Error()
			}
			if !strings.Contains(out, "ask again") || strings.Contains(out, "SECRET") {
				t.Errorf("after the swap: %q; want it refused", out)
			}
		})
	}
}

// Progress, questions and replay cross: a running bash command's
// output arrives as updates, a tool's question reaches the caller's
// elicitor and its answer the tool, and the replay claim is the tool's
// (safe for status, unknown for dax's).
func TestProgressQuestionsAndReplayCross(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	r := s.connect(t, mcpclient.WithClaims(), mcpclient.WithElicitation())

	var mu sync.Mutex
	var updates []string
	bash := remoteTool(t, r, "bash")
	res, err := bash.Execute(ctx, agenttool.Call{ID: "b1", Args: json.RawMessage(`{"command":"echo hi"}`), OnUpdate: func(u agenttool.Result) {
		mu.Lock()
		updates = append(updates, u.Output.String())
		mu.Unlock()
	}})
	if err != nil || !strings.Contains(res.Output.String(), "hi") {
		t.Fatalf("bash = %+v, %v", res, err)
	}
	mu.Lock()
	if len(updates) == 0 || !strings.Contains(strings.Join(updates, ""), "hi") {
		t.Errorf("updates %q", updates)
	}
	mu.Unlock()

	var asked agenttool.Elicitation
	ectx := agenttool.ContextWithElicitor(ctx, func(_ context.Context, q agenttool.Elicitation) (agenttool.Answer, error) {
		asked = q
		return agenttool.Answer{Action: agenttool.ActionAccept, Content: json.RawMessage(`{"pick":"b"}`)}, nil
	})
	res, err = remoteTool(t, r, "ask").Execute(ectx, agenttool.Call{ID: "a1", Args: json.RawMessage(`{}`)})
	if err != nil || !strings.Contains(res.Output.String(), "accept") || !strings.Contains(res.Output.String(), `"b"`) || asked.Message != "Which?" {
		t.Errorf("ask = %+v, %v; asked %+v", res, err, asked)
	}

	if got := agenttool.ReplayOf(ctx, remoteTool(t, r, "status"), json.RawMessage(`{}`)); got != agenttool.ReplaySafe {
		t.Errorf("status replay %v", got)
	}
	if got := agenttool.ReplayOf(ctx, remoteTool(t, r, "read"), json.RawMessage(`{"path":"x"}`)); got != agenttool.ReplayUnknown {
		t.Errorf("read replay %v", got)
	}
}

// A client that did not take the claims gets tools that make none, and
// asks the executor nothing; one that did asks.
func TestWithoutClaimsNothingIsAsked(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	r := s.connect(t)
	read := remoteTool(t, r, "read")
	if agenttool.IsFactual(read) {
		t.Error("read is factual without WithClaims")
	}
	agenttool.FactsOf(ctx, read, json.RawMessage(`{"path":"x"}`))
	agenttool.ReplayOf(ctx, read, json.RawMessage(`{"path":"x"}`))
	// x does not exist; the call runs and fails in the executor.
	read.Execute(ctx, agenttool.Call{ID: "r1", Args: json.RawMessage(`{"path":"x"}`)})
	if n := s.facts.Load(); n != 0 {
		t.Errorf("%d facts requests without claims", n)
	}

	claimed := remoteTool(t, s.connect(t, mcpclient.WithClaims()), "read")
	if !agenttool.IsFactual(claimed) {
		t.Fatal("read is not factual with WithClaims")
	}
	agenttool.FactsOf(ctx, claimed, json.RawMessage(`{"path":"x"}`))
	if n := s.facts.Load(); n == 0 {
		t.Error("the counter saw no facts request with claims")
	}
}

// Two tools of one name cannot both be in the capability.
func TestTwoToolsOfOneNameAreRefused(t *testing.T) {
	env := toolEnv(t)
	twice := extension.Extension{Name: "twice", Tools: func(extension.ToolEnv) []agenttool.Tool { return []agenttool.Tool{plain("read")} }}
	if _, _, err := NewServer(ServeOptions{Name: "dax"}, []extension.Extension{coding.New(0), twice}, env); err == nil || !strings.Contains(err.Error(), `"read"`) {
		t.Errorf("err = %v", err)
	}
}
