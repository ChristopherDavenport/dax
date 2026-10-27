package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/agentworkspace/remote"

	"github.com/ChristopherDavenport/dax/internal/authn"
	"github.com/ChristopherDavenport/dax/internal/config"
	"github.com/ChristopherDavenport/dax/policy"
)

// A servers workspace stands beside the session's own: its offered
// server, holding a secret, is connected as mcp__p__<tool>, redacted,
// while the file tools and bash act in the session's workspace and
// touch nothing of the servers workspace's directory; the header names
// it by its identity and the servers it offered, and -list's
// ServersWorkspaceOf reads that back.
func TestAServersWorkspaceBesideTheSessions(t *testing.T) {
	t.Setenv("DAX_TEST_MCP_SERVER", "1")
	t.Setenv(testSecretEnv, testSecret)
	for _, tc := range []struct {
		name  string
		serve func(*testing.T, workspace.Workspace, ...remote.Scope) remote.Workspace
	}{{"served", serveWS}, {"through a hub", tunnelWS}} {
		t.Run(tc.name, func(t *testing.T) {
			servers := t.TempDir()
			sw := tc.serve(t, offeringOf(t, servers, serversFile(t, testSecretEnv)), remote.ScopeServers)
			model := &scripted{calls: [][2]string{
				{"write", `{"path":"made.txt","content":"hi\n"}`},
				{"bash", `{"command":"echo hi > ran.txt"}`},
				{"mcp__p__secret", `{}`},
			}}
			o := options(t, model)
			o.ServersWorkspace = sw
			o.Policy = &policy.Settings{Builtin: true, Fallback: "allow"}
			s, err := New(t.Context(), o)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := promptOn(t.Context(), s, "go", nil); err != nil {
				t.Fatal(err)
			}
			out := outputs(s)
			if len(out) != 3 || !strings.HasPrefix(out[2], "secret=[[REDACTED "+testSecretEnv+"]]") || strings.Contains(out[2], testSecret) {
				t.Errorf("outputs %q", out)
			}
			for _, f := range []string{"made.txt", "ran.txt"} {
				if _, err := os.Stat(filepath.Join(o.Dir, f)); err != nil {
					t.Errorf("%s is not in the session's workspace: %v", f, err)
				}
				if _, err := os.Stat(filepath.Join(servers, f)); err == nil {
					t.Errorf("%s was made in the servers workspace", f)
				}
			}
			// The project is the session's workspace, whatever offers
			// the servers.
			if got := s.Info().Workspace; got.Kind != workspace.KindLocal || got.Root != o.Dir {
				t.Errorf("the session's workspace %+v", got)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			sums, err := List(t.Context(), o.Root, o.Dir)
			if err != nil || len(sums) != 1 {
				t.Fatalf("listed %d, %v", len(sums), err)
			}
			d, names, ok := ServersWorkspaceOf(sums[0].Header)
			if want := sw.Descriptor(); !ok || d.Kind != want.Kind || d.Ref != want.Ref || d.Ref == "" || d.Root != "" || !slices.Equal(names, []string{"p"}) {
				t.Errorf("recorded %+v %v %v; want %s %s and [p]", d, names, ok, want.Kind, want.Ref)
			}
			raw := string(sums[0].Header.Extra[headerServers])
			if strings.Contains(raw, testSecret) || strings.Contains(raw, servers) {
				t.Errorf("the header holds %s", raw)
			}
		})
	}
	// A session with none records none.
	o := options(t, &scripted{})
	s, err := New(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	sums, err := List(t.Context(), o.Root, o.Dir)
	if err != nil || len(sums) != 1 {
		t.Fatalf("listed %d, %v", len(sums), err)
	}
	if _, _, ok := ServersWorkspaceOf(sums[0].Header); ok {
		t.Error("a session without a servers workspace records one")
	}
}

// A servers workspace whose grant is more than servers is refused, as
// is the point of it: with exec the session reads a running server's
// environment, with write it changes the server, with read it reads
// what the server keeps. So are one that offers
// no servers, one that is the session's own workspace, and one offering
// a name the session's workspace offers or the user's config names.
func TestAServersWorkspaceIsRefused(t *testing.T) {
	t.Setenv("DAX_TEST_MCP_SERVER", "1")
	offered := func(t *testing.T, scopes ...remote.Scope) remote.Workspace {
		return serveWS(t, offeringOf(t, t.TempDir(), serversFile(t)), scopes...)
	}
	for _, tc := range []struct {
		name string
		set  func(t *testing.T, o *Options)
		want string
	}{
		{"exec", func(t *testing.T, o *Options) {
			o.ServersWorkspace = offered(t, remote.ScopeRead, remote.ScopeExec, remote.ScopeServers)
		}, "grants more than servers (read,exec,servers)"},
		{"write", func(t *testing.T, o *Options) {
			o.ServersWorkspace = offered(t, remote.ScopeRead, remote.ScopeWrite, remote.ScopeServers)
		}, "grants more than servers (read,write,servers)"},
		{"exec through a hub", func(t *testing.T, o *Options) {
			o.ServersWorkspace = tunnelWS(t, offeringOf(t, t.TempDir(), serversFile(t)), remote.ScopeRead, remote.ScopeExec, remote.ScopeServers)
		}, "grants more than servers (read,exec,servers)"},
		{"read", func(t *testing.T, o *Options) {
			o.ServersWorkspace = offered(t, remote.ScopeRead, remote.ScopeServers)
		}, "grants more than servers (read,servers)"},
		{"read through a hub", func(t *testing.T, o *Options) {
			o.ServersWorkspace = tunnelWS(t, offeringOf(t, t.TempDir(), serversFile(t)), remote.ScopeRead, remote.ScopeServers)
		}, "grants more than servers (read,servers)"},
		{"no servers", func(t *testing.T, o *Options) {
			o.ServersWorkspace = offered(t, remote.ScopeRead)
		}, "offers no MCP servers: its grant lacks servers"},
		{"the session's own", func(t *testing.T, o *Options) {
			ws := offered(t, remote.ScopeServers)
			o.Workspace, o.ServersWorkspace = ws, ws
		}, "is the session's workspace"},
		{"a name the workspace offers", func(t *testing.T, o *Options) {
			o.Workspace = offered(t, remote.ScopeServers)
			o.ServersWorkspace = offered(t, remote.ScopeServers)
		}, "mcp p: the workspace and the servers workspace remote"},
		{"a name the config gives", func(t *testing.T, o *Options) {
			o.ServersWorkspace = offered(t, remote.ScopeServers)
			o.MCP = []MCPServer{{Name: "p", Command: os.Args[0], Host: true}}
		}, "mcp p: a server your config names and one the workspace offers have the name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := options(t, &scripted{})
			tc.set(t, &o)
			s, err := New(t.Context(), o)
			if err == nil {
				s.Close()
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
	// /mcp add takes no name the servers workspace offers.
	o := options(t, &scripted{})
	o.ServersWorkspace = offered(t, remote.ScopeServers)
	s, err := New(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.AddMCP(t.Context(), "p", os.Args[0]); err == nil || !strings.Contains(err.Error(), "the workspace's server p offers tools under that name") {
		t.Errorf("/mcp add p: %v", err)
	}
}

// A servers workspace that drops is shown as a server that stopped:
// the next call to its server fails with the restart's notice, warned
// and recorded as any offered server's, and the session, whose own
// workspace is untouched, goes on.
func TestAServersWorkspaceThatDrops(t *testing.T) {
	t.Setenv("DAX_TEST_MCP_SERVER", "1")
	const token = "the-served-token"
	h, err := remote.NewHandler(offeringOf(t, t.TempDir(), serversFile(t)), remote.WithAuthenticator(authn.New(map[string]remote.Principal{
		token: {Name: "dax", Scopes: []remote.Scope{remote.ScopeServers}},
	}).Authenticator(remote.ErrUnauthenticated)))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	sw, err := remote.Dial(t.Context(), srv.URL, remote.WithHTTPClient(bearer(t, srv.URL, token)))
	if err != nil {
		t.Fatal(err)
	}
	defer sw.Close()

	var warned syncBuffer
	defer CaptureWarnings(&warned)()
	o := options(t, &scripted{})
	o.ServersWorkspace = sw
	s, err := New(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	if out := leak(t, s, "mcp__p__ping"); out != "pong" {
		t.Fatalf("ping = %q", out)
	}
	// The servers workspace goes away: its processes end, and nothing
	// answers at its address.
	h.Close()
	srv.CloseClientConnections()
	srv.Close()
	out := callAfterEnd(t.Context(), t, s, nil, "mcp__p__ping", true)
	if !strings.HasPrefix(out, "Error: ") || !strings.Contains(out, "could not be restarted; the next request tries again") {
		t.Errorf("a call after the drop: %q", out)
	}
	if got := warned.String(); !strings.Contains(got, "dax: MCP server p: ") {
		t.Errorf("warnings %q", got)
	}
	// The session's own workspace is untouched.
	rd, ok := s.Kit.LookupTool("read")
	if !ok {
		t.Fatal("no read tool")
	}
	res, err := rd.Execute(t.Context(), agenttool.Call{ID: "r", Args: json.RawMessage(`{"path":"AGENTS.md"}`)})
	if err != nil || !strings.Contains(res.Output.Text, "Run go test") {
		t.Errorf("read after the drop: %v, %v", res.Output.Text, err)
	}
	// Its close is clean: the server's session had ended, and the
	// failed restart reported it already. The record is kept whole.
	id := s.ID()
	if err := s.Close(); err != nil {
		t.Errorf("close after the drop: %v", err)
	}
	path, err := Project(context.Background(), o.Root, id, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	recs := restartsIn(t, data)
	if len(recs) == 0 || recs[0].Server != "p" || recs[0].OK || recs[0].Error == "" {
		t.Errorf("recorded restarts %+v", recs)
	}
}

// The servers workspace's grant is the scopes a served workspace's
// client reports (config.ServersAlone): a workspace that is not served
// reports none, and is refused even when it offers servers.
func TestAServersWorkspaceThatIsNotServedIsRefused(t *testing.T) {
	o := options(t, &scripted{})
	local, err := workspace.NewLocal(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	o.ServersWorkspace = startingOffer{local}
	if s, err := New(t.Context(), o); err == nil || !strings.Contains(err.Error(), "is not a served workspace") {
		if s != nil {
			s.Close()
		}
		t.Errorf("err = %v", err)
	}
	if config.ServersOnly(o.ServersWorkspace) || config.ServersAlone(o.ServersWorkspace) {
		t.Error("a workspace that is not served is servers-only")
	}
}

// startingOffer is a workspace that starts processes and offers a
// server.
type startingOffer struct{ *workspace.Local }

func (startingOffer) Servers(context.Context) ([]workspace.Server, error) {
	return []workspace.Server{{Name: "p"}}, nil
}

func (startingOffer) StartServer(context.Context, string, io.Writer) (workspace.Process, error) {
	return nil, io.EOF
}

// namedOffer is a servers-only stand-in that offers one server by name.
type namedOffer struct {
	workspace.Workspace
	name string
}

func (n namedOffer) Servers(context.Context) ([]workspace.Server, error) {
	return []workspace.Server{{Name: n.name}}, nil
}

func (namedOffer) StartServer(context.Context, string, io.Writer) (workspace.Process, error) {
	return nil, io.EOF
}

// A name agentworkspace does not allow a server is refused from any
// provider given in Options, a servers workspace's or the session's
// own: "u_" would have tools mcp__u___<tool>, which box.confine takes
// for a unit u's.
func TestAnOfferedNameThatIsNoServersIsRefused(t *testing.T) {
	for _, name := range []string{"u_", "U", "a__b", ""} {
		o := options(t, &scripted{})
		local, err := workspace.NewLocal(o.Dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		o.Workspace = namedOffer{struct{ workspace.Workspace }{local}, name}
		s, err := New(t.Context(), o)
		if err == nil {
			s.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "which is not a server's name") {
			t.Errorf("%q: err = %v", name, err)
		}
		local.Close()
	}
}

// Resuming with another servers workspace, or none, or one where the
// session had none, says so, as resuming in another workspace does; the
// header keeps the one the session started with.
func TestResumingWithAnotherServersWorkspaceSaysSo(t *testing.T) {
	gh := &workspace.Descriptor{Kind: workspace.KindRemote, Ref: "http://127.0.0.1:7781"}
	other := &workspace.Descriptor{Kind: workspace.KindRemote, Ref: "http://127.0.0.1:7782"}
	with := agentsession.Header{Extra: map[string]json.RawMessage{headerServers: json.RawMessage(`{"kind":"remote","ref":"http://127.0.0.1:7781","servers":["gh"]}`)}}
	for _, tc := range []struct {
		name string
		h    agentsession.Header
		now  *workspace.Descriptor
		want string
	}{
		{"the same", with, gh, ""},
		{"none either time", agentsession.Header{}, nil, ""},
		{"another", with, other, "session's servers were recorded from servers workspace remote http://127.0.0.1:7781, continuing with remote http://127.0.0.1:7782"},
		{"none now", with, nil, "session's servers were recorded from servers workspace remote http://127.0.0.1:7781, continuing without one"},
		{"none then", agentsession.Header{}, gh, "session was recorded without a servers workspace, continuing with remote http://127.0.0.1:7781"},
	} {
		if got := serversResumed(tc.h, tc.now); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}

	// Through Resume.
	t.Setenv("DAX_TEST_MCP_SERVER", "1")
	o := options(t, &scripted{})
	o.ServersWorkspace = serveWS(t, offeringOf(t, t.TempDir(), serversFile(t)), remote.ScopeServers)
	s, err := New(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID()
	s.Close()
	var warned syncBuffer
	defer CaptureWarnings(&warned)()
	o.ServersWorkspace = nil
	r, err := Resume(t.Context(), o, id)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	if got := warned.String(); !strings.Contains(got, "dax: session's servers were recorded from servers workspace remote http://127.0.0.1:") || !strings.Contains(got, ", continuing without one\n") {
		t.Errorf("warnings %q", got)
	}
}
