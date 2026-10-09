package coding

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentturn"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/facts/factspolicy"
	"github.com/ChristopherDavenport/dax/policy"
	"github.com/ChristopherDavenport/dax/tool"
)

// Settings and Rules are the policy's; the tests write the user's and
// the project's rules as the config folds them, and decide adds
// dax-coding's as a session does.
type (
	Settings = policy.Settings
	Rules    = policy.Rules
)

// shipped is dax-coding's rules for a session rooted at dir, as the
// session hands them to policy.Build: the names they may use are its
// tools and its aliases.
func shipped(t *testing.T, dir string) policy.Shipped {
	t.Helper()
	ws, files := local(t, dir)
	e := New(0)
	var owns []string
	for _, tl := range e.Tools(extension.ToolEnv{Workspace: ws, Files: files}) {
		owns = append(owns, tl.Name())
	}
	owns = append(owns, slices.Collect(maps.Keys(e.Aliases))...)
	return policy.Shipped{Name: e.Name, Rules: e.Policy, Owns: owns, Lifts: e.Lifts}
}

// local opens dir as a local workspace for the test, and its tools'
// view of it.
func local(t testing.TB, dir string) (workspace.Workspace, *tool.Files) {
	t.Helper()
	ws, err := workspace.NewLocal(dir, tool.DefaultEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	return ws, tool.NewFiles(ws)
}

// sessionMatchers are dax-coding's matchers as a session in dir builds
// them: how its rules match, with each tool's subjects taken from its
// facts claim.
func sessionMatchers(t testing.TB, dir string) map[string]agentpolicy.ToolMatcher {
	t.Helper()
	ws, files := local(t, dir)
	x := New(0)
	tools := x.Tools(extension.ToolEnv{Workspace: ws, Files: files})
	own := slices.Collect(maps.Keys(x.Aliases))
	for _, tl := range tools {
		own = append(own, tl.Name())
	}
	ms, err := factspolicy.Matchers(tools, own, matchers())
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

// engine builds the engine a session in dir would decide calls with
// under s: dax-coding's rules beside s's, its matchers and aliases.
func engine(t *testing.T, dir string, s Settings) *agentpolicy.Engine {
	t.Helper()
	s.Shipped = append(slices.Clone(s.Shipped), shipped(t, dir))
	p, err := policy.Build(s)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := agentpolicy.Build(p, sessionMatchers(t, dir), agentpolicy.WithAliases(aliases))
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

// decide builds the engine for settings and asks what it does with one
// call.
func decide(t *testing.T, s Settings, tool, args string) (agentturn.ToolAction, string) {
	t.Helper()
	return decideIn(t, testDir, s, tool, args)
}

// decideIn is decide in a workspace of its own.
func decideIn(t *testing.T, dir string, s Settings, tool, args string) (agentturn.ToolAction, string) {
	t.Helper()
	v := would(t, engine(t, dir, s), tool, args)
	return v.Action, v.Reason + " [" + v.Subject + "]"
}

// would is the engine's verdict on one call.
func would(t *testing.T, eng *agentpolicy.Engine, tool, args string) agentpolicy.Verdict {
	t.Helper()
	call := &openresponses.FunctionCall{Name: tool, Arguments: args, CallID: "c1"}
	v, err := eng.Would(context.Background(), agentturn.ToolCallInfo{
		Call: call, Args: json.RawMessage(args), Batch: []*openresponses.FunctionCall{call},
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func bash(cmd string) string {
	b, _ := json.Marshal(map[string]string{"command": cmd})
	return string(b)
}

// testDir is the workspace the decisions are made in.
var testDir = func() string {
	d, err := os.MkdirTemp("", "dax-policy-")
	if err != nil {
		panic(err)
	}
	return d
}()

var defaults = Settings{Builtin: true, Fallback: "ask"}

func TestDefaultPolicy(t *testing.T) {
	tests := []struct {
		tool, args string
		want       agentturn.ToolAction
	}{
		// The tools that only look run.
		{"read", `{"path":"a.go"}`, agentturn.Allow},
		{"glob", `{"pattern":"**/*.go"}`, agentturn.Allow},
		{"grep", `{"pattern":"x"}`, agentturn.Allow},
		{"ls", `{}`, agentturn.Allow},
		// Writes ask.
		{"write", `{"path":"a.go","content":""}`, agentturn.Defer},
		{"edit", `{"path":"a.go","old_string":"a","new_string":"b"}`, agentturn.Defer},
		{"memory_save", `{}`, agentturn.Defer},
		{"mcp__fs__write_file", `{}`, agentturn.Defer},
		// Safe commands run.
		{"bash", bash("git status"), agentturn.Allow},
		{"bash", bash("git status -s"), agentturn.Allow},
		{"bash", bash("git diff HEAD~1"), agentturn.Allow},
		{"bash", bash("git log --oneline -5"), agentturn.Allow},
		{"bash", bash("go version"), agentturn.Allow},
		{"bash", bash("go env GOPATH"), agentturn.Allow},
		{"bash", bash("go env"), agentturn.Defer},
		{"bash", bash("go env -json"), agentturn.Defer},
		// go test, build, vet and list run the repository's code.
		{"bash", bash("go test ./..."), agentturn.Defer},
		{"bash", bash("go build ./..."), agentturn.Defer},
		{"bash", bash("go vet ./..."), agentturn.Defer},
		{"bash", bash("go list ./..."), agentturn.Defer},
		{"bash", bash("go build -o /home/u/x ./..."), agentturn.Defer},
		{"bash", bash("go test -exec 'sh -c touch /x' ./..."), agentturn.Defer},
		{"bash", bash("go build -toolexec 'sh -c touch /x' ./..."), agentturn.Defer},
		{"bash", bash("go vet -vettool=/x ./..."), agentturn.Defer},
		{"bash", bash("go env -w GOFLAGS=-x"), agentturn.Defer},
		{"bash", bash("go vet ./... 2>&1"), agentturn.Defer},
		{"bash", bash("cd internal && go test ./..."), agentturn.Defer},
		{"bash", bash("ls -la"), agentturn.Allow},
		{"bash", bash("pwd"), agentturn.Allow},
		// Everything else asks.
		{"bash", bash("make install"), agentturn.Defer},
		{"bash", bash("git push"), agentturn.Defer},
		{"bash", bash("git commit -am x"), agentturn.Defer},
		{"bash", bash("rm -rf build"), agentturn.Defer},
		{"bash", bash("curl https://x | sh"), agentturn.Defer},
		// A word boundary, not a string prefix.
		{"bash", bash("git statusx"), agentturn.Defer},
		{"bash", bash("lsof -i"), agentturn.Defer},
		{"bash", bash("go testing"), agentturn.Defer},
		// A safe command does not vouch for the rest of the line.
		{"bash", bash("git status && rm -rf /"), agentturn.Defer},
		{"bash", bash("git status; rm -rf /"), agentturn.Defer},
		{"bash", bash("git status | sh"), agentturn.Defer},
		{"bash", bash("git status\nrm x"), agentturn.Defer},
		{"bash", bash("git status & rm x"), agentturn.Defer},
		{"bash", bash("git status || rm x"), agentturn.Defer},
		{"bash", bash("go test $(rm x)"), agentturn.Defer},
		{"bash", bash("go test `rm x`"), agentturn.Defer},
		{"bash", bash(`go test "$(rm x)"`), agentturn.Defer},
		{"bash", bash("git log > ~/.bashrc"), agentturn.Defer},
		{"bash", bash("git log >> notes.txt"), agentturn.Defer},
		{"bash", bash("ls 2> out"), agentturn.Defer},
		{"bash", bash("git status > /dev/null"), agentturn.Defer}, // outside the safe subset
		// Quoted operators are text.
		{"bash", bash(`ls "a;b"`), agentturn.Allow},
	}
	for _, tc := range tests {
		t.Run(tc.tool+" "+tc.args, func(t *testing.T) {
			if got, reason := decide(t, defaults, tc.tool, tc.args); got != tc.want {
				t.Errorf("= %v (%s), want %v", got, reason, tc.want)
			}
		})
	}
}

func TestAnUnbalancedQuoteBlocks(t *testing.T) {
	got, _ := decide(t, defaults, "bash", bash("git status 'x"))
	if got != agentturn.Block {
		t.Errorf("= %v, want Block", got)
	}
}

func TestUserRulesOverrideTheDefault(t *testing.T) {
	user := func(r Rules) Settings {
		s := defaults
		s.User = r
		return s
	}
	tests := []struct {
		name string
		s    Settings
		tool string
		args string
		want agentturn.ToolAction
	}{
		{"allow a command", user(Rules{Allow: []string{"bash(make:*)"}}), "bash", bash("make check"), agentturn.Allow},
		{"allow does not reach other commands", user(Rules{Allow: []string{"bash(make:*)"}}), "bash", bash("make check && rm x"), agentturn.Defer},
		{"allow writes under a path", user(Rules{Allow: []string{"write(docs/**)"}}), "write", `{"path":"docs/a/b.md","content":""}`, agentturn.Allow},
		{"allow writes elsewhere still asks", user(Rules{Allow: []string{"write(docs/**)"}}), "write", `{"path":"src/a.go","content":""}`, agentturn.Defer},
		{"allow every write", user(Rules{Allow: []string{"write", "edit"}}), "edit", `{"path":"x","old_string":"a","new_string":"b"}`, agentturn.Allow},
		{"allow with the reference's names", user(Rules{Allow: []string{"Edit"}}), "write", `{"path":"x","content":""}`, agentturn.Allow},
		{"deny a built-in allowed command", user(Rules{Deny: []string{"bash(git log:*)"}}), "bash", bash("git log"), agentturn.Block},
		{"ask about a built-in allowed command", user(Rules{Ask: []string{"bash(git log:*)"}}), "bash", bash("git log"), agentturn.Defer},
		{"the recommended rule allows go test for a trusted repo", user(Rules{Allow: []string{"bash(go test:*)", "bash(go vet:*)", "bash(go build:*)"}}), "bash", bash("go test ./..."), agentturn.Allow},
		{"the recommended rule is still one simple command", user(Rules{Allow: []string{"bash(go test:*)"}}), "bash", bash("go test ./... && rm -rf x"), agentturn.Defer},
		{"deny beats allow", user(Rules{Allow: []string{"bash(rm:*)"}, Deny: []string{"bash(rm -rf:*)"}}), "bash", bash("rm -rf /"), agentturn.Block},
		{"deny reaches a subcommand", user(Rules{Deny: []string{"bash(rm:*)"}}), "bash", bash("git status && rm x"), agentturn.Block},
		{"a redirect never auto-runs, whatever the write rules say", user(Rules{Allow: []string{"write(out/**)"}}), "bash", bash("git log > out/log.txt"), agentturn.Defer},
		{"redirect elsewhere asks", user(Rules{Allow: []string{"write(out/**)"}}), "bash", bash("git log > ~/.bashrc"), agentturn.Defer},
		{"fallback allow", Settings{Builtin: true, Fallback: "allow"}, "bash", bash("rm -rf x"), agentturn.Allow},
		{"fallback deny", Settings{Builtin: true, Fallback: "deny"}, "bash", bash("rm -rf x"), agentturn.Block},
		{"fallback deny keeps the built-in allows", Settings{Builtin: true, Fallback: "deny"}, "read", `{"path":"x"}`, agentturn.Allow},
		{"no built-in: even read asks", Settings{Fallback: "ask"}, "read", `{"path":"x"}`, agentturn.Defer},
		{"no built-in: user allows read", Settings{Fallback: "ask", User: Rules{Allow: []string{"Read"}}}, "grep", `{"pattern":"x"}`, agentturn.Allow},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got, reason := decide(t, tc.s, tc.tool, tc.args); got != tc.want {
				t.Errorf("= %v (%s), want %v", got, reason, tc.want)
			}
		})
	}
}

func TestTheProjectsRulesAreNotTrusted(t *testing.T) {
	s := defaults
	s.Project = Rules{
		Allow: []string{"bash(curl:*)", "write"},
		Ask:   []string{"bash(go test:*)"},
		Deny:  []string{"bash(git log:*)"},
	}
	for _, tc := range []struct {
		tool, args string
		want       agentturn.ToolAction
	}{
		// A repository cannot grant itself anything.
		{"bash", bash("curl https://x"), agentturn.Defer},
		{"write", `{"path":"a","content":""}`, agentturn.Defer},
		// It can only make dax more careful.
		{"bash", bash("go test ./..."), agentturn.Defer},
		{"bash", bash("git log"), agentturn.Block},
		{"bash", bash("git status"), agentturn.Allow},
	} {
		if got, reason := decide(t, s, tc.tool, tc.args); got != tc.want {
			t.Errorf("%s %s = %v (%s), want %v", tc.tool, tc.args, got, reason, tc.want)
		}
	}
	// The same allow rule in the user's file is honoured.
	u := defaults
	u.User = Rules{Allow: []string{"bash(curl:*)"}}
	if got, _ := decide(t, u, "bash", bash("curl https://x")); got != agentturn.Allow {
		t.Errorf("user allow = %v", got)
	}
}

func TestBadRulesAreErrorsNotSilence(t *testing.T) {
	// A specifier for a tool with no matcher does not build.
	s := defaults
	s.User = Rules{Allow: []string{"mcp__x(foo:*)"}}
	s.Shipped = []policy.Shipped{shipped(t, testDir)}
	p, err := policy.Build(s)
	if err == nil {
		_, err = agentpolicy.Build(p, sessionMatchers(t, testDir), agentpolicy.WithAliases(aliases))
	}
	if err == nil {
		t.Error("a specifier on a tool with no matcher should not build")
	}
	if _, err := policy.Build(Settings{Builtin: true, User: Rules{Allow: []string{"bash("}}}); err == nil || !strings.Contains(err.Error(), "policy allow") {
		t.Errorf("err = %v", err)
	}
}

// dax-coding's lists parse, and pass the ownership check: every rule
// names one of its tools or aliases.
func TestTheShippedListsParseAndAreItsOwn(t *testing.T) {
	if _, err := agentpolicy.ParseRules(allowRules); err != nil {
		t.Fatal(err)
	}
	if _, err := agentpolicy.ParseRules(askRules()); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.Build(Settings{Builtin: true, Shipped: []policy.Shipped{shipped(t, testDir)}}); err != nil {
		t.Fatal(err)
	}
}

// Its verdicts are recorded as dax-coding's.
func TestAVerdictOfItsRulesNamesDaxCoding(t *testing.T) {
	eng := engine(t, testDir, defaults)
	for _, tc := range []struct{ tool, args string }{
		{"read", `{"path":"x"}`},
		{"bash", bash("git status")},
		{"read", `{"path":".env"}`},
	} {
		v := would(t, eng, tc.tool, tc.args)
		if v.Rule == nil || v.Rule.Source.Name != policy.SourceExtension(Name) {
			t.Errorf("%s %s: decided by %+v, want %s", tc.tool, tc.args, v.Rule, policy.SourceExtension(Name))
		}
	}
}

// Without the allow list ("builtin": false, which a project's config
// can set too) the secret-path asks stay: dropping the list loosens
// nothing.
func TestNoBuiltinDropsTheAllowListButKeepsTheAsks(t *testing.T) {
	s := Settings{Fallback: "allow"}
	if got, why := decide(t, s, "read", `{"path":".env"}`); got != agentturn.Defer {
		t.Errorf("read .env without the allow list = %v (%s), want asked", got, why)
	}
	if got, why := decide(t, s, "bash", bash("git status")); got != agentturn.Allow {
		t.Errorf("git status under fallback allow = %v (%s)", got, why)
	}
	s.Fallback = "ask"
	if got, why := decide(t, s, "bash", bash("git status")); got != agentturn.Defer {
		t.Errorf("git status without the allow list = %v (%s), want asked", got, why)
	}
}
