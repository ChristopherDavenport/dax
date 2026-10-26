package agent

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/agentworkspace/remote"
	"github.com/ChristopherDavenport/agentworkspace/tunnel"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ChristopherDavenport/dax/internal/authn"
	"github.com/ChristopherDavenport/dax/internal/config"
	"github.com/ChristopherDavenport/dax/internal/offer"
	"github.com/ChristopherDavenport/dax/policy"
	"github.com/ChristopherDavenport/dax/tool"
)

// testSecretEnv is the variable the test server's secret tool reports,
// on its output and its standard error.
const testSecretEnv = "DAX_TEST_SECRET"

// testSecret is its value in the tests that give it.
const testSecret = "the-placement-secret-4d1f"

// addPlacementTools adds the test server's ping, which answers the same
// wherever the server runs, and secret, which writes the value of
// testSecretEnv to its standard error, a line ending "end", and
// returns it.
func addPlacementTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "ping", Description: "answer pong"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, struct{}{}, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "secret", Description: "report the secret"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
			v := os.Getenv(testSecretEnv)
			os.Stderr.WriteString("stderr secret=[" + v + "] end\n")
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "secret=[" + v + "]"}}}, struct{}{}, nil
		})
}

// servedEnv is a served workspace's environment in these tests: this
// machine's less its credentials, and the test server's switch.
func servedEnv() []string { return append(tool.DefaultEnv(nil), "DAX_TEST_MCP_SERVER=1") }

// offeringOf is dir offering the servers file names, as dax workspace
// -servers offers them: the file read by config.ParseOffered and
// offered through internal/offer.
func offeringOf(t *testing.T, dir, file string) *workspace.Offering {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := config.ParseOffered(data, file)
	if err != nil {
		t.Fatal(err)
	}
	var servers []offer.Server
	for _, name := range slices.Sorted(maps.Keys(parsed)) {
		m := parsed[name]
		servers = append(servers, offer.Server{Name: name, Description: m.Description, Command: m.Command, SecretEnv: m.SecretEnv})
	}
	local, err := workspace.NewLocal(dir, servedEnv())
	if err != nil {
		t.Fatal(err)
	}
	o, err := offer.Offer(local, servers, os.LookupEnv)
	if err != nil {
		local.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { o.Close() })
	return o
}

// serveWS serves ws over remote behind one token holding scopes, as dax
// workspace -listen does, and dials it.
func serveWS(t *testing.T, ws workspace.Workspace, scopes ...remote.Scope) remote.Workspace {
	t.Helper()
	const token = "the-served-token"
	h, err := remote.NewHandler(ws, remote.WithAuthenticator(authn.New(map[string]remote.Principal{
		token: {Name: "dax", Scopes: scopes},
	}).Authenticator(remote.ErrUnauthenticated)))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { h.Close() })
	c, err := remote.Dial(t.Context(), srv.URL, remote.WithHTTPClient(bearer(t, srv.URL, token)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// tunnelWS serves ws through a hub under scopes, as dax workspace
// -connect does to a session's workspace.hub, and returns the hub's
// workspace.
func tunnelWS(t *testing.T, ws workspace.Workspace, scopes ...remote.Scope) remote.Workspace {
	t.Helper()
	const token = "the-tunnel-token-of-at-least-32-bytes"
	hub, err := tunnel.NewHub(tunnel.WithEnrollment(authn.New(map[string]string{token: "build"}).Authenticator(errors.New("no such token"))))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { hub.Close() })
	sctx, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- tunnel.Serve(sctx, "ws"+strings.TrimPrefix(srv.URL, "http"), ws, tunnel.WithScopes(scopes...),
			tunnel.WithHeader(func(context.Context) (http.Header, error) {
				return http.Header{"Authorization": {"Bearer " + token}}, nil
			}))
	}()
	t.Cleanup(func() {
		stop()
		select {
		case <-served:
		case <-time.After(10 * time.Second):
			t.Error("tunnel.Serve did not stop")
		}
	})
	wctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	c, err := hub.Workspace(wctx, "build")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// serversFile writes a -servers file offering the test binary as p,
// holding secrets when it names any, and returns its path.
func serversFile(t *testing.T, secrets ...string) string {
	t.Helper()
	m := `{"command": "` + os.Args[0] + `", "description": "the test server"`
	if len(secrets) > 0 {
		m += `, "secret_env": ["` + strings.Join(secrets, `", "`) + `"]`
	}
	file := filepath.Join(t.TempDir(), "servers.json")
	write(t, file, `{"mcp_servers": {"p": `+m+`}}}`)
	return file
}

// placements are the places one MCP server called p runs, each set on
// o: configured as a command on this machine, configured as a command
// in a served workspace, offered by a served workspace (-servers) over
// -workspace URL, and offered through a hub (workspace.hub). secrets
// are the variables the server is given as secrets; an offered server
// holding any is served under servers and read alone, as dax workspace
// requires.
func placements(secrets ...string) []struct {
	name string
	set  func(t *testing.T, o *Options)
} {
	scopes := []remote.Scope{remote.ScopeRead, remote.ScopeWrite, remote.ScopeExec, remote.ScopeServers}
	if len(secrets) > 0 {
		scopes = []remote.Scope{remote.ScopeRead, remote.ScopeServers}
	}
	return []struct {
		name string
		set  func(t *testing.T, o *Options)
	}{
		{"a command on this machine", func(t *testing.T, o *Options) {
			o.MCP = []MCPServer{{Name: "p", Command: os.Args[0], Host: true, SecretEnv: secrets}}
		}},
		{"a command in a served workspace", func(t *testing.T, o *Options) {
			local, err := workspace.NewLocal(t.TempDir(), servedEnv())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { local.Close() })
			o.Workspace = serveWS(t, local, remote.ScopeRead, remote.ScopeWrite, remote.ScopeExec)
			o.MCP = []MCPServer{{Name: "p", Command: os.Args[0], SecretEnv: secrets}}
		}},
		{"offered by a served workspace", func(t *testing.T, o *Options) {
			o.Workspace = serveWS(t, offeringOf(t, t.TempDir(), serversFile(t, secrets...)), scopes...)
		}},
		{"offered through a hub", func(t *testing.T, o *Options) {
			o.Workspace = tunnelWS(t, offeringOf(t, t.TempDir(), serversFile(t, secrets...)), scopes...)
		}},
	}
}

// One MCP server has one interface wherever it runs: the session's
// mcpStart connects it on this machine, in a served workspace, offered
// by one, and offered through a hub, and each time its tools are the
// same, named mcp__p__<tool>, and a call answers the same.
func TestOneMCPServerInEveryPlacement(t *testing.T) {
	t.Setenv("DAX_TEST_MCP_SERVER", "1")
	var want []string
	for _, pl := range placements() {
		t.Run(pl.name, func(t *testing.T) {
			o := options(t, &scripted{})
			pl.set(t, &o)
			s, err := New(t.Context(), o)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			var got []string
			for _, n := range toolNames(s) {
				if strings.HasPrefix(n, "mcp__") {
					got = append(got, n)
				}
			}
			slices.Sort(got)
			if want == nil {
				want = got
			}
			if !slices.Equal(got, want) || !slices.Contains(got, "mcp__p__ping") {
				t.Errorf("tools %v, want %v", got, want)
			}
			if out := leak(t, s, "mcp__p__ping"); out != "pong" {
				t.Errorf("ping = %q", out)
			}
		})
	}
}

// waitFor waits, at most ten seconds, for buf to hold s, which a
// process writes: what is waited on is the process's own output.
func waitFor(t *testing.T, buf *syncBuffer, s string) string {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(buf.String(), s); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("waited for %q; have %q", s, buf.String())
		}
	}
	return buf.String()
}

// A server given a secret (secret_env) never shows it to the session,
// wherever it runs: it is given the value, and what it writes, its
// output and its standard error, has it redacted before the session
// reads any of it.
func TestASecretHoldingServerIsRedactedInEveryPlacement(t *testing.T) {
	t.Setenv("DAX_TEST_MCP_SERVER", "1")
	t.Setenv(testSecretEnv, testSecret)
	for _, pl := range placements(testSecretEnv) {
		t.Run(pl.name, func(t *testing.T) {
			var warned syncBuffer
			defer CaptureWarnings(&warned)()
			o := options(t, &scripted{})
			pl.set(t, &o)
			s, err := New(t.Context(), o)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			out := leak(t, s, "mcp__p__secret")
			if out != "secret=[[REDACTED "+testSecretEnv+"]]" {
				t.Errorf("the server's output: %q", out)
			}
			stderr := waitFor(t, &warned, "] end")
			if strings.Contains(stderr, testSecret) || !strings.Contains(stderr, "stderr secret=[[REDACTED "+testSecretEnv+"]] end") {
				t.Errorf("the server's standard error: %q", stderr)
			}
		})
	}
}

// Without secret_env the variable is a secret like any other the
// session's children lose: the server is not given it.
func TestASecretIsGivenOnlyToTheServerNamingIt(t *testing.T) {
	t.Setenv("DAX_TEST_MCP_SERVER", "1")
	t.Setenv(testSecretEnv, testSecret)
	var warned syncBuffer
	defer CaptureWarnings(&warned)()
	o := options(t, &scripted{})
	o.MCP = []MCPServer{
		{Name: "held", Command: os.Args[0], SecretEnv: []string{testSecretEnv}},
		{Name: "other", Command: os.Args[0]},
	}
	o.Policy = &policy.Settings{Builtin: true, Fallback: "allow"}
	o.Extensions = o.Extensions[:1]
	s, err := New(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if out := leak(t, s, "mcp__other__secret"); out != "secret=[]" {
		t.Errorf("a server without secret_env was given the secret: %q", out)
	}
	if !slices.Contains(o.secrets(), testSecretEnv) {
		t.Errorf("secrets %v leave secret_env in children's environments", o.secrets())
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// A variable that is not set fails the start, naming it.
	t.Setenv(testSecretEnv, "")
	o2 := options(t, &scripted{})
	o2.MCP = []MCPServer{{Name: "held", Command: os.Args[0], SecretEnv: []string{testSecretEnv}}}
	if s2, err := New(t.Context(), o2); err == nil || !strings.Contains(err.Error(), "secret_env "+testSecretEnv+" is not set") {
		if s2 != nil {
			s2.Close()
		}
		t.Errorf("an unset secret: %v", err)
	}
}

// The session refuses a secret_env a project's server names, one for a
// URL, and one that is another credential's variable or one PassEnv
// hands every child.
func TestSecretEnvRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*Options)
		want string
	}{
		{"a project's", func(o *Options) {
			o.MCP = []MCPServer{{Name: "p", Command: "x", Project: true, SecretEnv: []string{"S"}}}
		}, "a project's server is given none of your secrets"},
		{"a url's", func(o *Options) {
			o.MCP = []MCPServer{{Name: "p", URL: "https://mcp.example", SecretEnv: []string{"S"}}}
		}, "secret_env is for a command"},
		{"the key's", func(o *Options) {
			o.KeyEnv = "MY_KEY"
			o.MCP = []MCPServer{{Name: "p", Command: "x", SecretEnv: []string{"MY_KEY"}}}
		}, "holds the model's key"},
		{"the workspace's token", func(o *Options) {
			o.MCP = []MCPServer{{Name: "p", Command: "x", SecretEnv: []string{config.WorkspaceTokenEnv}}}
		}, "is a served workspace's token"},
		{"the hub's token", func(o *Options) {
			o.MCP = []MCPServer{{Name: "p", Command: "x", SecretEnv: []string{config.TunnelTokenEnv}}}
		}, "is a served workspace's token"},
		{"a token_env", func(o *Options) {
			o.MCP = []MCPServer{{Name: "u", URL: "https://mcp.example", TokenEnv: "T"}, {Name: "p", Command: "x", SecretEnv: []string{"T"}}}
		}, "is a server's token_env"},
		{"pass_env's", func(o *Options) {
			o.PassEnv = []string{"S"}
			o.MCP = []MCPServer{{Name: "p", Command: "x", SecretEnv: []string{"S"}}}
		}, "is in PassEnv"},
	} {
		o := options(t, &scripted{})
		tc.set(&o)
		if s, err := New(t.Context(), o); err == nil || !strings.Contains(err.Error(), tc.want) {
			if s != nil {
				s.Close()
			}
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}

// A workspace served under servers alone, at an address or through a
// hub, gives the session its offered servers and nothing else: the
// servers' tools work, the file tools fail with the refusal, and bash
// is offered no process.
func TestAServersOnlyWorkspace(t *testing.T) {
	t.Setenv("DAX_TEST_MCP_SERVER", "1")
	for _, tc := range []struct {
		name  string
		serve func(*testing.T, workspace.Workspace, ...remote.Scope) remote.Workspace
	}{{"served", serveWS}, {"through a hub", tunnelWS}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, filepath.Join(dir, "README.md"), "# private\n")
			ws := tc.serve(t, offeringOf(t, dir, serversFile(t)), remote.ScopeServers)
			if _, ok := ws.(workspace.Starter); ok {
				t.Fatal("a servers-only workspace is a Starter")
			}
			if _, ok := ws.(workspace.ServerProvider); !ok {
				t.Fatal("a servers-only workspace offers no servers")
			}
			if _, err := fs.Stat(ws.FS(), "README.md"); !errors.Is(err, remote.ErrForbidden) {
				t.Fatalf("a stat without read: %v", err)
			}
			model := &scripted{calls: [][2]string{
				{"read", `{"path":"README.md"}`},
				{"write", `{"path":"made.txt","content":"hi\n"}`},
				{"bash", `{"command":"echo hi > ran.txt"}`},
			}}
			o := options(t, model)
			o.Workspace = ws
			o.Policy = &policy.Settings{Builtin: true, Fallback: "allow"}
			s, err := New(t.Context(), o)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if out := leak(t, s, "mcp__p__ping"); out != "pong" {
				t.Errorf("ping = %q", out)
			}
			if _, err := promptOn(t.Context(), s, "go", nil); err != nil {
				t.Fatal(err)
			}
			out := outputs(s)
			if len(out) != 3 {
				t.Fatalf("outputs %q", out)
			}
			for i, scope := range []string{"read", "write", "exec"} {
				if !strings.Contains(out[i], "the principal lacks the "+scope+" scope") || strings.Contains(out[i], "# private") {
					t.Errorf("call %d: %q, want the refusal for %s", i, out[i], scope)
				}
			}
			for _, f := range []string{"made.txt", "ran.txt"} {
				if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
					t.Errorf("%s was made", f)
				}
			}
		})
	}
}
