package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ChristopherDavenport/dax/internal/config"
	"github.com/ChristopherDavenport/dax/internal/executor"
	"github.com/ChristopherDavenport/dax/policy"
)

// boxHostAnswer is the client's end of a connection to an executor, with the
// handshake's answer given box under the executor's capability, as a
// box host's relay gives it.
type boxHostAnswer struct {
	mcp.Transport
	box json.RawMessage
}

func (b boxHostAnswer) Connect(ctx context.Context) (mcp.Connection, error) {
	c, err := b.Transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return boxHostConn{Connection: c, box: b.box}, nil
}

type boxHostConn struct {
	mcp.Connection
	box json.RawMessage
}

func (c boxHostConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	m, err := c.Connection.Read(ctx)
	if r, ok := m.(*jsonrpc.Response); ok && len(r.Result) > 0 {
		var res map[string]any
		if json.Unmarshal(r.Result, &res) == nil {
			if caps, ok := res["capabilities"].(map[string]any); ok {
				if exp, ok := caps["experimental"].(map[string]any); ok {
					if x, ok := exp[executor.CapabilityKey].(map[string]any); ok {
						var box any
						json.Unmarshal(c.box, &box)
						x["box"] = box
						r.Result, _ = json.Marshal(res)
					}
				}
			}
		}
	}
	return m, err
}

// The box the tests' box host resolved: the workspace under gVisor with
// no internet and its git guard on; a unit with a read-only project and
// no internet; one with internet and a secret; one under runc.
const confineBoxJSON = `{"profile":"default","units":[
	{"name":"workspace","kind":"workspace","runtime":"runsc","files":"project rw","internet":"off","guard":"on: .git read-only"},
	{"name":"files","kind":"mcp","runtime":"runsc","files":"project ro, state rw","internet":"off"},
	{"name":"gh","kind":"mcp","runtime":"runsc","files":"none","internet":"allow","allow":["api.github.com"],"secrets":["GITHUB_TOKEN"]},
	{"name":"weak","kind":"mcp","runtime":"runc","files":"none","internet":"off"}]}`

// confineSession is a session whose tools run in an executor behind a
// box host that resolved raw (nil: a plain executor), under
// box.confine mode, with settings edit applied to its policy.
func confineSession(t *testing.T, raw string, mode string, edit func(*policy.Settings)) *Session {
	t.Helper()
	s, err := confineSessionWith(t, raw, mode, edit, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// confineSessionWith is confineSession whose box offers only the units
// named in offer when offer is not nil, and with mcp configured too; it
// returns New's error.
func confineSessionWith(t *testing.T, raw string, mode string, edit func(*policy.Settings), offer []string, servers ...MCPServer) (*Session, error) {
	t.Helper()
	ctx := context.Background()
	// The units' servers are what the box offers (servers.list and
	// servers.start, which a box host answers); here the box's workspace
	// offers each MCP unit as the test binary's own MCP server.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var offered []workspace.OfferedServer
	units := boxUnits(json.RawMessage(raw))
	if offer != nil {
		units = offer
	}
	for _, u := range units {
		offered = append(offered, workspace.OfferedServer{Server: workspace.Server{Name: u},
			Command: workspace.Command{Args: []string{self}, Env: []string{"DAX_TEST_MCP_SERVER=1"}}})
	}
	b := newRemoteBoxOffering(t, offered)
	for _, d := range []string{"src", ".git/hooks", ".github/workflows"} {
		if err := os.MkdirAll(filepath.Join(b.dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(b.dir, "src/a.go"), "package a\n")
	write(t, filepath.Join(b.dir, ".github/workflows/ci.yml"), "on: push\n")
	st, ct := mcp.NewInMemoryTransports()
	ss, err := b.srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	var tr mcp.Transport = ct
	if raw != "" {
		tr = boxHostAnswer{Transport: ct, box: json.RawMessage(raw)}
	}
	ex, err := connectExecutor(ctx, dialOnce(tr), executor.ConnectOptions{Name: "dax", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ex.Close() })
	o := remoteOptions(t, &scripted{}, ex)
	o.BoxConfine = mode
	o.MCP = servers
	if edit != nil {
		edit(o.Policy)
	}
	s, err := New(ctx, o)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { s.Close() })
	return s, nil
}

// unitTool stands in for a unit's MCP tool: only its name matters to
// the claim, which is made by the box host's envelope for the unit.
func unitTool(name string) agenttool.Tool {
	return agenttool.New(name, "a unit's tool", func(context.Context, struct{}) (string, error) { return "", nil })
}

// would is the session's policy's verdict on a call of name with args,
// as the main agent's (or, with child, a sub-agent's) decision.
func would(t *testing.T, s *Session, child, name, args string) agentturn.ToolAction {
	t.Helper()
	ctx := context.Background()
	if child != "" {
		ctx = childDecision(ctx, child)
	}
	tl, ok := s.Kit.LookupTool(name)
	if !ok {
		tl = unitTool(name)
	}
	v, err := s.Kit.Engine().Would(ctx, agentturn.ToolCallInfo{
		Call: &openresponses.FunctionCall{Name: name, Arguments: args}, Tool: tl, Args: json.RawMessage(args),
	})
	if err != nil {
		t.Fatalf("%s %s: %v", name, args, err)
	}
	return v.Action
}

func bashArgs(line string) string {
	b, _ := json.Marshal(map[string]string{"command": line})
	return string(b)
}

const (
	allow = agentturn.Allow
	ask   = agentturn.Defer
	deny  = agentturn.Block
)

// box.confine, by mode, over the box's tools: what runs unasked, what
// still asks, and that nothing the policy allows without it asks with
// it.
func TestBoxConfineDecisions(t *testing.T) {
	type call struct {
		child, name, args string
	}
	calls := map[string]call{
		"ls src":              {"", "bash", bashArgs("ls src")},
		"git status":          {"", "bash", bashArgs("git status")},
		"go test":             {"", "bash", bashArgs("go test ./...")},
		"rm -rf src":          {"", "bash", bashArgs("rm -rf src")},
		"sh -c hook":          {"", "bash", bashArgs("sh -c 'echo x > .git/hooks/pre-commit'")},
		"redirect hook":       {"", "bash", bashArgs("echo x > .git/hooks/pre-commit")},
		"rm -rf .git":         {"", "bash", bashArgs("rm -rf .git")},
		"write src/a.go":      {"", "write", `{"path":"src/a.go","content":"package a\n"}`},
		"write .git/config":   {"", "write", `{"path":".git/config","content":"x"}`},
		"write .envrc":        {"", "write", `{"path":".envrc","content":"x"}`},
		"edit ci.yml":         {"", "edit", `{"path":".github/workflows/ci.yml","old_string":"push","new_string":"pull_request"}`},
		"unit files":          {"", "mcp__files__read_file", `{}`},
		"unit gh":             {"", "mcp__gh__create_issue", `{}`},
		"unit runc":           {"", "mcp__weak__x", `{}`},
		"explore rm -rf src":  {"explore", "bash", bashArgs("rm -rf src")},
		"explore ls src":      {"explore", "bash", bashArgs("ls src")},
		"task write src/a.go": {"task", "write", `{"path":"src/a.go","content":"x"}`},
	}
	// Without box.confine, and with a plain executor whatever the mode,
	// every call is decided as it is today.
	today := map[string]agentturn.ToolAction{
		"ls src": allow, "git status": allow, "go test": ask, "rm -rf src": ask, "sh -c hook": ask,
		"redirect hook": ask, "rm -rf .git": ask, "write src/a.go": ask, "write .git/config": ask,
		"write .envrc": ask, "edit ci.yml": ask, "unit files": ask, "unit gh": ask, "unit runc": ask,
		"explore rm -rf src": ask, "explore ls src": allow, "task write src/a.go": ask,
	}
	with := func(changes map[string]agentturn.ToolAction) map[string]agentturn.ToolAction {
		out := map[string]agentturn.ToolAction{}
		for k, v := range today {
			out[k] = v
		}
		for k, v := range changes {
			out[k] = v
		}
		return out
	}
	for _, tc := range []struct {
		name string
		box  string
		mode string
		edit func(*policy.Settings)
		want map[string]agentturn.ToolAction
	}{
		{name: "off", box: confineBoxJSON, mode: config.ConfineOff, want: today},
		{name: "plain executor", mode: config.ConfineWorkspace, want: today},
		{name: "units", box: confineBoxJSON, mode: config.ConfineUnits, want: with(map[string]agentturn.ToolAction{
			"unit files": allow,
		})},
		{name: "workspace", box: confineBoxJSON, mode: config.ConfineWorkspace, want: with(map[string]agentturn.ToolAction{
			"go test": allow, "rm -rf src": allow, "write src/a.go": allow, "unit files": allow,
		})},
		// The workspace is claimed only behind its git guard: a box host
		// that does not say it holds the repository's .git read-only (an
		// older dax), or says there is no repository, gets
		// the workspace's tools asked as today; its units are unchanged.
		{name: "workspace, no guard said", box: strings.Replace(confineBoxJSON, `,"guard":"on: .git read-only"`, ``, 1), mode: config.ConfineWorkspace,
			want: with(map[string]agentturn.ToolAction{"unit files": allow})},
		{name: "workspace, not a repository", box: strings.Replace(confineBoxJSON, `"guard":"on: .git read-only"`, `"guard":"none: not a git repository"`, 1), mode: config.ConfineWorkspace,
			want: with(map[string]agentturn.ToolAction{"unit files": allow})},
		{name: "workspace, a user's deny", box: confineBoxJSON, mode: config.ConfineWorkspace,
			edit: func(p *policy.Settings) { p.User.Deny = []string{"bash(rm:*)"} },
			want: with(map[string]agentturn.ToolAction{
				"go test": allow, "rm -rf src": deny, "rm -rf .git": deny, "write src/a.go": allow, "unit files": allow,
				"explore rm -rf src": deny,
			})},
		{name: "workspace, a project's bare asks", box: confineBoxJSON, mode: config.ConfineWorkspace,
			edit: func(p *policy.Settings) { p.Project.Ask = []string{"bash", "mcp__*"} },
			// The repository's bare asks are not turned into allows: what
			// they cover asks as without box.confine, and write and edit,
			// which share the workspace's claim with bash, are not
			// claimed either.
			want: with(map[string]agentturn.ToolAction{"ls src": ask, "git status": ask, "explore ls src": ask})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := confineSession(t, tc.box, tc.mode, tc.edit)
			for k, c := range calls {
				if got := would(t, s, c.child, c.name, c.args); got != tc.want[k] {
					t.Errorf("%s: %v, want %v", k, got, tc.want[k])
				}
			}
		})
	}
}

// A confined call names what confined it in its verdict, and one the
// policy allows anyway says which rule.
func TestBoxConfineReasons(t *testing.T) {
	s := confineSession(t, confineBoxJSON, config.ConfineWorkspace, nil)
	ctx := context.Background()
	for _, tc := range []struct{ name, args, want string }{
		{"bash", bashArgs("go test ./..."), "confined by box:runsc:workspace"},
		{"bash", bashArgs("git status"), "confined by allowed by bash(git status:*)"},
		{"mcp__files__read_file", `{}`, "confined by box:runsc:files"},
	} {
		tl, ok := s.Kit.LookupTool(tc.name)
		if !ok {
			tl = unitTool(tc.name)
		}
		v, err := s.Kit.Engine().Would(ctx, agentturn.ToolCallInfo{Call: &openresponses.FunctionCall{Name: tc.name, Arguments: tc.args}, Tool: tl, Args: json.RawMessage(tc.args)})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(v.Reason, tc.want) {
			t.Errorf("%s %s: reason %q, want one naming %q", tc.name, tc.args, v.Reason, tc.want)
		}
	}
}

// A server whose tools would be named as a unit's is refused, so the
// claim, made by name, is always the unit's.
func TestAServerCannotTakeAUnitsName(t *testing.T) {
	s := confineSession(t, confineBoxJSON, config.ConfineUnits, nil)
	for _, name := range []string{"files", "files_"} {
		if _, err := s.AddMCP(context.Background(), name, "server"); err == nil || !strings.Contains(err.Error(), "server files") {
			t.Errorf("/mcp add %s: %v", name, err)
		}
	}
	if !collidesWithOffered("files_", "files") || collidesWithOffered("files-x", "files") || collidesWithOffered("file", "files") {
		t.Error("collidesWithOffered")
	}
}

// What a unit's envelope must hold to be claimed.
func TestContainedFiles(t *testing.T) {
	for files, want := range map[string]bool{
		"none": true, "project ro": true, "state rw": true, "project ro, state rw": true,
		"project rw": false, "project rw, state rw": false, "": false, "everything": false,
	} {
		if got := containedFiles(files); got != want {
			t.Errorf("containedFiles(%q) = %v, want %v", files, got, want)
		}
	}
	for rt, want := range map[string]bool{"runsc": true, "kata": true, "io.containerd.kata.v2": true, "runc": false, "io.containerd.runc.v2": false, "": false} {
		if got := sandboxed(rt); got != want {
			t.Errorf("sandboxed(%q) = %v, want %v", rt, got, want)
		}
	}
}

// Behind an older box host, which passes servers.list through to the
// executor and so offers none, a session whose handshake names units
// fails to open, saying so, rather than start without its units and
// leave their names to another server.
func TestAnOlderBoxHostFailsTheSession(t *testing.T) {
	_, err := confineSessionWith(t, confineBoxJSON, config.ConfineUnits, nil, []string{})
	if err == nil || !strings.Contains(err.Error(), "the box host offers no server for unit files; it is older than this dax, update it") {
		t.Fatalf("New = %v", err)
	}
}

// A server placed on the host, or one at a URL, may not take a unit's
// name: box.confine would claim its tools.
func TestAHostServerCannotTakeAUnitsName(t *testing.T) {
	for _, m := range []MCPServer{
		{Name: "files", Command: "server", Host: true},
		{Name: "files_", Command: "server", Host: true},
		{Name: "files", URL: "https://evil.example/mcp"},
	} {
		if s, err := confineSessionWith(t, confineBoxJSON, config.ConfineUnits, nil, nil, m); err == nil || !strings.Contains(err.Error(), "rename the server") {
			if s != nil {
				s.Close()
			}
			t.Errorf("%+v beside the unit files: %v", m, err)
		}
	}
}

// The confiner claims a unit's tools only when the unit is a server the
// session connected, beside its envelope.
func TestTheConfinerClaimsOnlyConnectedUnits(t *testing.T) {
	s := confineSession(t, confineBoxJSON, config.ConfineUnits, nil)
	raw := json.RawMessage(confineBoxJSON)
	c, err := newConfiner(config.ConfineUnits, raw, s.set, nil, nil, []string{"files", "gh", "weak"})
	if err != nil || c == nil || c.units["files"] == "" {
		t.Fatalf("with files connected: %+v, %v", c, err)
	}
	c, err = newConfiner(config.ConfineUnits, raw, s.set, nil, nil, []string{"gh", "weak"})
	if err != nil {
		t.Fatal(err)
	}
	if c != nil && c.units["files"] != "" {
		t.Errorf("files claimed though no server of it was connected: %+v", c.units)
	}
}
