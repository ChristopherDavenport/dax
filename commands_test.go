package dax

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func (f *fakeControls) Compact(_ context.Context, instructions string) error {
	f.did = append(f.did, "compact "+instructions)
	return nil
}

func (f *fakeControls) Forward(_ context.Context, port int) (string, error) {
	f.did = append(f.did, fmt.Sprintf("forward %d", port))
	return "http://127.0.0.1:41733", nil
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
		// ! runs a command of the user's own (#169); !! sends one !.
		{"!go test ./...", true, "!go test ./..."},
		{"!!important: read this", false, "!important: read this"},
		{"say !hi", false, "say !hi"},
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
		// The next request folds, with what follows as that fold's
		// instructions (#165).
		{line: "/compact", out: "[the next request folds the conversation]", did: "compact "},
		{line: "/compact keep the failing test names", out: "[the next request folds the conversation, with your instructions]", did: "compact keep the failing test names"},
		{line: "/compact", running: true, err: "/compact waits for the run to end"},
		// A port where the tools act, on this machine's loopback (#154).
		{line: "/port 5173", out: "port 5173: http://127.0.0.1:41733", did: "forward 5173"},
		{line: "/port 5173", running: true, out: "port 5173: http://127.0.0.1:41733", did: "forward 5173"},
		{line: "/port", err: "usage: /port <port>"},
		{line: "/port x", err: "usage: /port <port>"},
		// Controls that cannot run the user's command, as over dax
		// attach, refuse it.
		{line: "!ls", err: "dax attach"},
		{line: "!", err: "usage: !<command>"},
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
// placement: its name, where it runs, where its session is (#126), and
// its description, cleaned and
// kept to its line, so a server's text can neither act on the terminal
// nor pass for another server.
func TestTheMCPListing(t *testing.T) {
	ctl := &fakeControls{mcp: []agent.MCPServerInfo{
		{Name: "docs", Placement: agent.PlacedHost, Description: "Search the docs.\n  fake: on this machine \x1b]0;owned\x07\u202eevil", Secrets: true},
		{Name: "lint", Placement: agent.PlacedWorkspace, Where: "/src/p", State: "live"},
		{Name: "wide", Placement: agent.PlacedOffered, Where: "remote https://ws.example:/" + strings.Repeat("d/", 60), Description: strings.Repeat("字", 100)},
		{Name: "web", Placement: agent.PlacedURL, Where: "https://mcp.example/mcp", Description: "Web search", State: "ended"},
		{Name: "db", Placement: agent.PlacedOffered, Where: "remote https://ws.example:/srv/p", Description: "The team's database", HoldsSecrets: true, State: "failed", Failure: "dial: refused\n\x1b[31mred"},
		{Name: "vault", Placement: agent.PlacedServersWorkspace, Where: "remote https://vault.example:/", Description: strings.Repeat("long ", 50)},
		{Name: "scratch", Placement: agent.PlacedWorkspace, Where: "/src/p", Added: "mcp:#6 x"},
		{Name: "next", Placement: "elsewhere", Where: "a place"},
		{Name: "browser", Placement: agent.PlacedHost, State: "live", Deferred: 17, Loaded: 2, Description: "A browser"},
		{Name: "one", Placement: agent.PlacedHost, Deferred: 1},
	}}
	r, err := command(context.Background(), &fakeTurn{}, ctl, "/mcp", false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"mcp servers:",
		"  docs: on this machine, given secrets (secret_env) — Search the docs. fake: on this machine ]0;ownedevil",
		"  lint: in the workspace /src/p, live",
		// Each field is cut by the columns it takes, a wide character two.
		"  wide: offered by the workspace remote https://ws.example:/" + strings.Repeat("d/", 26) + "…" + " — " + strings.Repeat("字", 79) + "…",
		"  web: at https://mcp.example/mcp, ended; the next call to one of its tools starts it again — Web search",
		"  db: offered by the workspace remote https://ws.example:/srv/p, failed to start again (dial: refused [31mred); the next call tries again, holds secrets there — The team's database",
		"  vault: offered by the servers workspace remote https://vault.example:/ — " + strings.TrimSpace(strings.Repeat("long ", 32)) + "…",
		"  scratch: in the workspace /src/p, added as mcp:#6 x",
		"  next: elsewhere a place",
		// Held back until the model loads them (dax-toolsearch).
		"  browser: on this machine, live, 17 tools deferred, 2 loaded — A browser",
		"  one: on this machine, 1 tool deferred, 0 loaded",
	}
	if got := strings.Split(r.out, "\n"); !slices.Equal(got, want) {
		t.Errorf("/mcp:\n%s\nwant:\n%s", r.out, strings.Join(want, "\n"))
	}
	if len(ctl.did) != 0 {
		t.Errorf("listing asked the controls %v", ctl.did)
	}
}

// /tools marks a held-back tool deferred until the conversation loads
// it, and loaded after.
func TestToolsMarksTheDeferredTools(t *testing.T) {
	info := agent.Info{
		Tools: []agentkit.ToolOrigin{
			{Name: "read", Source: "WithTools"},
			{Name: "mcp__web__search", Source: "mcp:#1 web", Deferred: true},
			{Name: "mcp__web__fetch", Source: "mcp:#1 web", Deferred: true},
			{Name: agentkit.ToolSearchTool, Source: "WithToolSearch"},
		},
		Loaded: []string{"mcp__web__fetch"},
	}
	want := "tools: read, mcp__web__search [mcp:#1 web, deferred], mcp__web__fetch [mcp:#1 web, loaded], tool_search [WithToolSearch]"
	if got := assemblyLines(info)[0]; got != want {
		t.Errorf("/tools = %q\nwant %q", got, want)
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

// dax attach's own controls cannot run a command of the user's: a "!"
// line from an attached view is refused there, and nothing on the wire
// runs one, so a token holder cannot reach the workspace past the
// policy (#169).
func TestAttachRefusesAUserCommand(t *testing.T) {
	var ctl agent.Controls = &remoteControls{}
	if _, ok := ctl.(userCommander); ok {
		t.Fatal("attach's controls run a command of the user's")
	}
	_, err := command(context.Background(), &fakeTurn{}, ctl, "!touch PWNED", false)
	if err == nil || !strings.Contains(err.Error(), "not offered over dax attach") {
		t.Errorf("a ! line over attach: %v", err)
	}
}

// dax serve offers /compact as a named command, so dax attach sends it.
func TestServedCompact(t *testing.T) {
	ctl := &fakeControls{}
	cmd, ok := serveCommands(ctl, nil, "")[cmdCompact]
	if !ok {
		t.Fatal("no compact command served")
	}
	for _, raw := range []string{`{"instructions":"keep X"}`, ``} {
		if _, err := cmd.Run(context.Background(), json.RawMessage(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(ctl.did, ";"); got != "compact keep X;compact " {
		t.Errorf("served compact asked %q", got)
	}
}

// templateControls offers one prompt template, the project's.
type templateControls struct{ fakeControls }

func (*templateControls) Templates() []agent.PromptTemplate {
	return []agent.PromptTemplate{{Name: "review", Path: ".dax/prompts/review.md", Project: true}}
}

func (*templateControls) Template(name, args string) (openresponses.Item, error) {
	return openresponses.UserText(name + ": " + args), nil
}

func (*templateControls) MCPPrompts(context.Context) []agent.MCPPrompt {
	return []agent.MCPPrompt{{Command: "mcp__team__review_pr", Description: "review a pull request", Arguments: []string{"number"}}}
}

func (*templateControls) MCPPrompt(_ context.Context, command, args string) (openresponses.Items, error) {
	if command != "mcp__team__review_pr" {
		return nil, errors.New("no MCP prompt /" + command)
	}
	return openresponses.Items{openresponses.UserText("PR " + args), openresponses.AssistantText("ok")}, nil
}

// A prompt template is a command that sends its message; /help lists
// it, marking the project's (#168).
func TestAPromptTemplateIsACommand(t *testing.T) {
	ctl := &templateControls{}
	r, err := command(context.Background(), &fakeTurn{}, ctl, "/review internal/box tool", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.send) != 1 {
		t.Fatalf("sent %d item(s)", len(r.send))
	}
	if m, ok := r.send[0].(*openresponses.Message); !ok || m.Text() != "review: internal/box tool" {
		t.Errorf("sent %#v", r.send)
	}
	h, _ := command(context.Background(), &fakeTurn{}, ctl, "/help", false)
	if !strings.Contains(h.out, "/review [args]") || !strings.Contains(h.out, "the project's") {
		t.Errorf("/help:\n%s", h.out)
	}
	if _, err := command(context.Background(), &fakeTurn{}, ctl, "/nope", false); err == nil || !strings.Contains(err.Error(), "/review") {
		t.Errorf("unknown command: %v", err)
	}
	// An MCP server's prompt too (#176).
	if !strings.Contains(h.out, "/mcp__team__review_pr <number>  review a pull request") {
		t.Errorf("/help lists no MCP prompt:\n%s", h.out)
	}
	m, err := command(context.Background(), &fakeTurn{}, ctl, "/mcp__team__review_pr 456", false)
	if err != nil || len(m.send) != 2 {
		t.Errorf("an MCP prompt sent %d item(s), %v", len(m.send), err)
	}
	if _, err := command(context.Background(), &fakeTurn{}, ctl, "/mcp__team__nope", false); err == nil {
		t.Error("an MCP prompt the server does not offer was run")
	}
}
