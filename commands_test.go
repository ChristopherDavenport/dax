package dax

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/agent"
)

// fakeTurn records what a command asked of the session's control; the
// methods a command does not use are the nil interface's.
type fakeTurn struct {
	agentturn.Control
	aborted  bool
	followed []string
}

func (f *fakeTurn) Abort() { f.aborted = true }

func (f *fakeTurn) Queue(_ context.Context, mode agentturn.QueueMode, items ...openresponses.Item) error {
	if mode != agentturn.QueueFollowUp {
		return errors.New("queued as " + string(mode) + ", want a follow-up")
	}
	for _, it := range items {
		f.followed = append(f.followed, it.(*openresponses.Message).Text())
	}
	return nil
}

// fakeControls records what a command asked of the session's controls.
type fakeControls struct {
	did []string
	mcp []agent.MCPServerInfo // the servers Info lists; none when nil
	// unlisted has Info list no servers at all (a nil MCP), as a served
	// session's older dax does.
	unlisted bool
}

func (f *fakeControls) SetModel(name string) error {
	if name == "bad" {
		return errors.New("no such model \x1b]0;owned\x07")
	}
	f.did = append(f.did, "model "+name)
	return nil
}

func (f *fakeControls) SetThink(on bool) error {
	f.did = append(f.did, map[bool]string{true: "think on", false: "think off"}[on])
	return nil
}

func (f *fakeControls) AddMCP(_ context.Context, name, command string) (string, error) {
	f.did = append(f.did, "add "+name+" "+command)
	return "mcp__" + name, nil
}

func (f *fakeControls) RemoveMCP(label string) error {
	f.did = append(f.did, "remove "+label)
	return nil
}

func (f *fakeControls) Info() agent.Info {
	servers := f.mcp
	if servers == nil && !f.unlisted {
		servers = []agent.MCPServerInfo{}
	}
	return agent.Info{ID: "s1", Path: "/store/s1", Tools: []agentkit.ToolOrigin{{Name: "read", Source: "WithTools"}, {Name: "mcp__x__\x1b[31mevil", Source: "mcp"}}, MCP: servers}
}

func TestALineIsACommandWhenItStartsWithOneSlash(t *testing.T) {
	for _, c := range []struct {
		line    string
		command bool
		text    string
	}{
		{"/model x", true, "/model x"},
		{"/", true, "/"},
		{"//etc/hosts says what?", false, "/etc/hosts says what?"},
		{"// a comment", false, "/ a comment"},
		{"hello /model", false, "hello /model"},
	} {
		if got := isCommand(c.line); got != c.command {
			t.Errorf("isCommand(%q) = %v, want %v", c.line, got, c.command)
		}
		if !c.command {
			if got := userText(c.line); got != c.text {
				t.Errorf("userText(%q) = %q, want %q", c.line, got, c.text)
			}
		}
	}
}

func TestCommands(t *testing.T) {
	for _, c := range []struct {
		line    string
		running bool
		out     string // in the output
		err     string // in the error; empty for none
		did     string // what the controls were asked, joined by ";"
		quit    bool
		abort   bool
		follow  string
	}{
		{line: "/model glm", out: "[model glm from the next request]", did: "model glm"},
		{line: "/model bad", err: "no such model"},
		{line: "/model", err: "usage: /model <name>"},
		{line: "/model glm", running: true, err: "/model waits for the run to end"},
		{line: "/think on", did: "think on"},
		{line: "/think maybe", err: "usage: /think on|off"},
		{line: "/think off", running: true, err: "/think waits for the run to end"},
		{line: "/mcp add docs npx docs-server --port 1", out: "[mcp__docs added", did: "add docs npx docs-server --port 1"},
		{line: "/mcp add docs", err: "usage: /mcp add"},
		{line: "/mcp remove mcp__docs", out: "tools: read", did: "remove mcp__docs"},
		{line: "/mcp add docs x", running: true, err: "/mcp waits for the run to end"},
		{line: "/mcp list", out: "mcp servers: none"},
		{line: "/mcp", running: true, out: "mcp servers: none"},
		{line: "/mcp start docs", err: "usage: /mcp, /mcp add"},
		{line: "/mcp list docs", err: "usage: /mcp list, with nothing after it"},
		{line: "/tools", out: "tools: read, mcp__x__[31mevil [mcp]"},
		{line: "/tools", running: true, out: "tools: read"},
		{line: "/session", out: "s1 /store/s1"},
		{line: "/follow then test it", follow: "then test it", out: "for the next run"},
		{line: "/follow then test it", running: true, follow: "then test it", out: "[queued as follow-up]"},
		{line: "/follow", err: "usage: /follow <text>"},
		{line: "/abort", running: true, abort: true},
		{line: "/abort", err: "no run to abort"},
		{line: "/quit", quit: true},
		{line: "/exit", running: true, quit: true},
		{line: "/help", out: "//text"},
		{line: "/compact", err: "unknown command /compact"},
	} {
		turn, ctl := &fakeTurn{}, &fakeControls{}
		r, err := command(context.Background(), turn, ctl, c.line, c.running)
		name := c.line
		if c.running {
			name += " (running)"
		}
		switch {
		case c.err == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%s: error %v, want one with %q", name, err, c.err)
		}
		if !strings.Contains(r.out, c.out) {
			t.Errorf("%s: output %q, want it to have %q", name, r.out, c.out)
		}
		if strings.Contains(r.out, "\x1b") || (err != nil && strings.Contains(err.Error(), "\x1b")) {
			t.Errorf("%s: output %q or error %v is not cleaned", name, r.out, err)
		}
		if got := strings.Join(ctl.did, ";"); got != c.did {
			t.Errorf("%s: controls asked %q, want %q", name, got, c.did)
		}
		if r.quit != c.quit || turn.aborted != c.abort {
			t.Errorf("%s: quit %v abort %v, want %v %v", name, r.quit, turn.aborted, c.quit, c.abort)
		}
		if got := strings.Join(turn.followed, ";"); got != c.follow {
			t.Errorf("%s: followed %q, want %q", name, got, c.follow)
		}
	}
}

// /mcp lists each MCP server one line, the same way whatever its
// placement: its name, where it runs, and its description, cleaned and
// kept to its line, so a server's text can neither act on the terminal
// nor pass for another server.
func TestTheMCPListing(t *testing.T) {
	ctl := &fakeControls{mcp: []agent.MCPServerInfo{
		{Name: "docs", Placement: agent.PlacedHost, Description: "Search the docs.\n  fake: on this machine \x1b]0;owned\x07\u202eevil", Secrets: true},
		{Name: "lint", Placement: agent.PlacedWorkspace, Where: "/src/p"},
		{Name: "wide", Placement: agent.PlacedOffered, Where: "remote https://ws.example:/" + strings.Repeat("d/", 60), Description: strings.Repeat("字", 100)},
		{Name: "web", Placement: agent.PlacedURL, Where: "https://mcp.example/mcp", Description: "Web search"},
		{Name: "db", Placement: agent.PlacedOffered, Where: "remote https://ws.example:/srv/p", Description: "The team's database"},
		{Name: "vault", Placement: agent.PlacedServersWorkspace, Where: "remote https://vault.example:/", Description: strings.Repeat("long ", 50)},
		{Name: "scratch", Placement: agent.PlacedWorkspace, Where: "/src/p", Added: "mcp:#6 x"},
		{Name: "next", Placement: "elsewhere", Where: "a place"},
	}}
	r, err := command(context.Background(), &fakeTurn{}, ctl, "/mcp", false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"mcp servers:",
		"  docs: on this machine, given secrets (secret_env) — Search the docs. fake: on this machine ]0;ownedevil",
		"  lint: in the workspace /src/p",
		// Each field is cut by the columns it takes, a wide character two.
		"  wide: offered by the workspace remote https://ws.example:/" + strings.Repeat("d/", 26) + "…" + " — " + strings.Repeat("字", 79) + "…",
		"  web: at https://mcp.example/mcp — Web search",
		"  db: offered by the workspace remote https://ws.example:/srv/p — The team's database",
		"  vault: offered by the servers workspace remote https://vault.example:/ — " + strings.TrimSpace(strings.Repeat("long ", 32)) + "…",
		"  scratch: in the workspace /src/p, added as mcp:#6 x",
		"  next: elsewhere a place",
	}
	if got := strings.Split(r.out, "\n"); !slices.Equal(got, want) {
		t.Errorf("/mcp:\n%s\nwant:\n%s", r.out, strings.Join(want, "\n"))
	}
	if len(ctl.did) != 0 {
		t.Errorf("listing asked the controls %v", ctl.did)
	}
}

// A served session whose dax is older than /mcp sends no list of its
// servers; the view says so, rather than that it has none.
func TestAnUnlistedSessionIsNotSaidToHaveNoServers(t *testing.T) {
	r, err := command(context.Background(), &fakeTurn{}, &fakeControls{unlisted: true}, "/mcp", false)
	if err != nil {
		t.Fatal(err)
	}
	if r.out != "mcp servers: not listed; the served session's dax is older than /mcp" {
		t.Errorf("/mcp = %q", r.out)
	}
}
