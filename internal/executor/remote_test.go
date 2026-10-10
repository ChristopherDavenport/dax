package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agenttool/mcpserver"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ChristopherDavenport/dax/ext/coding"
)

// remote is Connect over s, in memory; the server's session is
// returned too, so a test can hang up on the client.
func (s executorServer) remote(t *testing.T) (*Remote, *sdk.ServerSession) {
	t.Helper()
	r, ss, err := connectTo(s.srv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(); ss.Close() })
	return r, ss
}

func connectTo(srv *sdk.Server) (*Remote, *sdk.ServerSession, error) {
	ctx := context.Background()
	st, ct := sdk.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		return nil, nil, err
	}
	r, err := Connect(ctx, ct, ConnectOptions{Name: "dax", Version: "test"})
	if err != nil {
		ss.Close()
		return nil, nil, err
	}
	return r, ss, nil
}

// The client builds the tools the executor runs in the capability's
// order, not the listing's, each with the extension, read-only and
// strict the capability says and the scheduling and claims the listing
// carries, and reports the executor's descriptor and extensions.
func TestConnectBuildsTheToolsInTheExecutorsOrder(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	r, _ := s.remote(t)
	tools, err := r.Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	byName := map[string]Tool{}
	for _, tl := range tools {
		names = append(names, tl.Name())
		byName[tl.Name()] = tl
	}
	if want := []string{"read", "write", "edit", "glob", "grep", "ls", "bash", "status", "deploy", "ask"}; !slices.Equal(names, want) {
		t.Errorf("order %v, want %v", names, want)
	}
	if b := byName["bash"]; b.Extension != coding.Name || !b.ReadOnly || !b.Factual || !b.Sequential {
		t.Errorf("bash %+v", b)
	}
	if st := byName["status"]; st.Extension != "acme" || !st.ReadOnly || st.Factual {
		t.Errorf("status %+v", st)
	}
	if d := byName["deploy"]; d.Definition.Strict == nil || !*d.Definition.Strict || d.Resource != "acme:deploys" || d.ReadOnly {
		t.Errorf("deploy %+v", d)
	}
	if d := byName["write"]; d.ReadOnly || !d.Factual {
		t.Errorf("write %+v", d)
	}
	if got := r.Descriptor(); got != testDescriptor(s.ws.Root()) {
		t.Errorf("descriptor %+v", got)
	}
	if got := r.Extensions(); !slices.Equal(got, []string{coding.Name, "acme"}) {
		t.Errorf("extensions %v", got)
	}
	if name, version := r.Server(); name != "dax" || version != "test" {
		t.Errorf("server %s %s", name, version)
	}
	if got := r.Replay(ctx, Call{Tool: "status", Call: agenttool.Call{Args: json.RawMessage(`{}`)}}); got != agenttool.ReplaySafe {
		t.Errorf("status replay %v", got)
	}
}

// fake is an MCP server that says what it is told: exp as its
// experimental capabilities (nil for none), and tools served by
// mcpserver (which answers the facts method) or, with plainSDK, by the
// SDK alone (which does not).
func fake(t *testing.T, exp map[string]any, plainSDK bool, tools ...agenttool.Tool) *sdk.Server {
	t.Helper()
	srv := sdk.NewServer(&sdk.Implementation{Name: "fake", Version: "1"}, &sdk.ServerOptions{
		Capabilities: &sdk.ServerCapabilities{Experimental: exp, Tools: &sdk.ToolCapabilities{}},
	})
	if plainSDK {
		for _, tl := range tools {
			sdk.AddTool(srv, &sdk.Tool{Name: tl.Name(), InputSchema: map[string]any{"type": "object"}},
				func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, struct{}, error) {
					return &sdk.CallToolResult{}, struct{}{}, nil
				})
		}
		return srv
	}
	if err := mcpserver.AddTools(srv, tools...); err != nil {
		t.Fatal(err)
	}
	return srv
}

// claiming is a tool that makes the facts claim.
func claiming(name string) agenttool.Tool {
	return plain(name, agenttool.WithFacts(func(context.Context, json.RawMessage) (agenttool.Facts, error) {
		return agenttool.Facts{}, nil
	}))
}

// A server that does not say what a session needs of an executor is
// refused at connect: without the facts method or the capability, of
// another version, naming no workspace or not serving its files (an
// executor of dax v0.0.6), with a start capability whose bounds are not
// positive, listing a tool the capability does not name
// or the reverse, or saying a tool claims facts that is listed without
// the claim (or the reverse). A session that took such
// a server's tools would match the model's raw arguments where it
// should match what the call touches.
func TestConnectRefusesAServerThatIsNotAnExecutor(t *testing.T) {
	desc := capabilityDescriptor{Kind: "container", Ref: "box", Root: "/work"}
	capOf := func(c capability) map[string]any { return map[string]any{CapabilityKey: c} }
	good := func(tools ...capabilityTool) capability {
		return capability{Version: CapabilityVersion, Descriptor: desc, Extensions: []string{"acme"}, Tools: tools, Files: &capabilityFiles{URITemplate: FilesURITemplate}}
	}
	for _, tc := range []struct {
		name  string
		srv   func(t *testing.T) *sdk.Server
		wants string
	}{
		{"a plain MCP server", func(t *testing.T) *sdk.Server {
			return fake(t, nil, true, plain("read"))
		}, "does not answer execution/facts"},
		{"the dax capability without the facts method", func(t *testing.T) *sdk.Server {
			return fake(t, capOf(good(capabilityTool{Name: "read", Extension: "acme"})), true, plain("read"))
		}, "does not answer execution/facts"},
		{"the facts method without the dax capability", func(t *testing.T) *sdk.Server {
			return fake(t, nil, false, plain("read"))
		}, "no " + CapabilityKey},
		{"another version", func(t *testing.T) *sdk.Server {
			c := good(capabilityTool{Name: "read", Extension: "acme"})
			c.Version = 2
			return fake(t, capOf(c), false, plain("read"))
		}, "version 2"},
		{"a capability that does not decode", func(t *testing.T) *sdk.Server {
			return fake(t, map[string]any{CapabilityKey: map[string]any{"version": "one"}}, false, plain("read"))
		}, CapabilityKey},
		{"no workspace", func(t *testing.T) *sdk.Server {
			c := good(capabilityTool{Name: "read", Extension: "acme"})
			c.Descriptor = capabilityDescriptor{}
			return fake(t, capOf(c), false, plain("read"))
		}, "names no workspace"},
		{"an executor that does not serve its files (dax v0.0.6)", func(t *testing.T) *sdk.Server {
			c := good(capabilityTool{Name: "read", Extension: "acme"})
			c.Files = nil
			return fake(t, capOf(c), false, plain("read"))
		}, "does not serve its workspace's files"},
		{"its files under another template", func(t *testing.T) *sdk.Server {
			c := good(capabilityTool{Name: "read", Extension: "acme"})
			c.Files.URITemplate = "file:///{path}"
			return fake(t, capOf(c), false, plain("read"))
		}, `serves its workspace's files under "file:///{path}"`},
		{"a start bound that is not positive", func(t *testing.T) *sdk.Server {
			c := good(capabilityTool{Name: "read", Extension: "acme"})
			c.Start = &capabilityStart{MaxProcesses: 32, MaxWriteBytes: 0, MaxReadBytes: 1}
			return fake(t, capOf(c), false, plain("read"))
		}, "not positive"},
		{"a listed tool the capability does not name", func(t *testing.T) *sdk.Server {
			return fake(t, capOf(good(capabilityTool{Name: "read", Extension: "acme"})), false, plain("read"), plain("rm"))
		}, `lists "rm"`},
		{"a named tool it does not list", func(t *testing.T) *sdk.Server {
			return fake(t, capOf(good(capabilityTool{Name: "read", Extension: "acme"}, capabilityTool{Name: "rm", Extension: "acme"})), false, plain("read"))
		}, `names "rm", which it does not list`},
		{"a tool named twice", func(t *testing.T) *sdk.Server {
			return fake(t, capOf(good(capabilityTool{Name: "read", Extension: "acme"}, capabilityTool{Name: "read", Extension: "acme"})), false, plain("read"))
		}, `names "read" twice`},
		{"a tool of an extension it does not name", func(t *testing.T) *sdk.Server {
			return fake(t, capOf(good(capabilityTool{Name: "read", Extension: "other"})), false, plain("read"))
		}, `extension "other"`},
		{"facts claimed but not listed with the claim", func(t *testing.T) *sdk.Server {
			return fake(t, capOf(good(capabilityTool{Name: "read", Extension: "acme", Facts: true})), false, plain("read"))
		}, "listed without the claim"},
		{"listed with a claim the capability does not name", func(t *testing.T) *sdk.Server {
			return fake(t, capOf(good(capabilityTool{Name: "read", Extension: "acme"})), false, claiming("read"))
		}, "does not name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, ss, err := connectTo(tc.srv(t))
			if err == nil {
				r.Close()
				ss.Close()
				t.Fatal("connected")
			}
			if !errors.Is(err, errNotExecutor) || !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("err = %v, want %q", err, tc.wants)
			}
		})
	}
	// The same server with everything right connects.
	r, ss, err := connectTo(fake(t, capOf(good(capabilityTool{Name: "read", Extension: "acme", Facts: true})), false, claiming("read")))
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	ss.Close()
}

// A reading of the facts that fails, takes too long or finds the
// executor gone is an error, which blocks the call; nothing is made up
// in its place.
func TestAFactsReadingThatCannotBeMadeIsAnError(t *testing.T) {
	ctx := context.Background()
	read := Call{Tool: "read", Call: agenttool.Call{ID: "r", Args: json.RawMessage(`{"path":"notes.txt"}`)}}

	t.Run("timeout", func(t *testing.T) {
		old := factsTimeout
		factsTimeout = 50 * time.Millisecond
		defer func() { factsTimeout = old }()
		s := newServer(t)
		s.srv.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
			return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
				if method == mcpserver.FactsMethod {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return next(ctx, method, req)
			}
		})
		r, _ := s.remote(t)
		start := time.Now()
		if f, err := r.Facts(ctx, read); err == nil {
			t.Errorf("facts %+v, want an error", f)
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Errorf("took %v", d)
		}
	})
	t.Run("gone", func(t *testing.T) {
		s := newServer(t)
		r, ss := s.remote(t)
		ss.Close()
		if f, err := r.Facts(ctx, read); err == nil {
			t.Errorf("facts %+v, want an error", f)
		}
		if res, err := r.Call(ctx, read); err == nil {
			t.Errorf("call %+v ran", res)
		}
	})
	t.Run("unknown tool", func(t *testing.T) {
		r, _ := newServer(t).remote(t)
		if _, err := r.Facts(ctx, Call{Tool: "nope"}); err == nil {
			t.Error("facts of a tool the executor does not run")
		}
	})
}

// Through the client, the stamp a decision's facts put on a call is
// checked in the executor: a read of notes.txt, read and stamped, then
// notes.txt swapped for a link to .env, is refused there.
func TestTheClientsStampIsCheckedInTheExecutor(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	os.WriteFile(filepath.Join(s.dir, "notes.txt"), []byte("notes\n"), 0o644)
	os.WriteFile(filepath.Join(s.dir, ".env"), []byte("SECRET=1\n"), 0o644)
	r, _ := s.remote(t)
	read := Call{Tool: "read", Call: agenttool.Call{ID: "r", Args: json.RawMessage(`{"path":"notes.txt"}`)}}
	f, err := r.Facts(ctx, read)
	if err != nil || !strings.Contains(string(f.Rewrite), "dax_stamp") {
		t.Fatalf("facts %+v, %v", f, err)
	}
	os.Remove(filepath.Join(s.dir, "notes.txt"))
	if err := os.Symlink(".env", filepath.Join(s.dir, "notes.txt")); err != nil {
		t.Fatal(err)
	}
	res, err := r.Call(ctx, Call{Tool: "read", Call: agenttool.Call{ID: "r", Args: f.Rewrite}})
	out := res.Output.String()
	if err != nil {
		out += err.Error()
	}
	if !strings.Contains(out, "ask again") || strings.Contains(out, "SECRET") {
		t.Errorf("after the swap: %q", out)
	}
}

// BatchFacts reads a response's calls in one request: each call's claim
// or its own error (a tool the executor does not run among them), and
// the request's failure, the executor gone, as the error of all.
func TestBatchFactsReadsTheCallsInOneRequest(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	os.WriteFile(filepath.Join(s.dir, "notes.txt"), []byte("notes\n"), 0o644)
	var requests atomic.Int32
	s.srv.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			if method == mcpserver.FactsMethod {
				requests.Add(1)
			}
			return next(ctx, method, req)
		}
	})
	r, ss := s.remote(t)
	calls := []Call{
		{Tool: "read", Call: agenttool.Call{Args: json.RawMessage(`{"path":"notes.txt"}`)}},
		{Tool: "nope", Call: agenttool.Call{Args: json.RawMessage(`{}`)}},
		{Tool: "read", Call: agenttool.Call{Args: json.RawMessage(`{"path":"other.txt"}`)}},
	}
	facts, errs, err := r.BatchFacts(ctx, calls)
	if err != nil {
		t.Fatal(err)
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
	if errs[0] != nil || !strings.Contains(string(facts[0].Rewrite), "dax_stamp") {
		t.Errorf("notes.txt: %+v, %v", facts[0], errs[0])
	}
	if errs[1] == nil {
		t.Error("a tool the executor does not run was read")
	}
	if errs[2] != nil || len(facts[2].Calls) == 0 {
		t.Errorf("other.txt: %+v, %v", facts[2], errs[2])
	}
	ss.Close()
	if _, _, err := r.BatchFacts(ctx, calls); err == nil {
		t.Error("the executor gone, BatchFacts gave no error")
	}
}
