package policy

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dex/internal/config"
)

// decide builds the engine for settings and asks what it does with one
// call.
func decide(t *testing.T, s config.PolicySettings, tool, args string) (agentturn.ToolAction, string) {
	t.Helper()
	return decideIn(t, testDir, s, tool, args)
}

// decideIn is decide in a workspace of its own.
func decideIn(t *testing.T, dir string, s config.PolicySettings, tool, args string) (agentturn.ToolAction, string) {
	t.Helper()
	p, err := Build(s)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := agentpolicy.Build(p, Matchers(dir, 0), Options()...)
	if err != nil {
		t.Fatal(err)
	}
	call := &openresponses.FunctionCall{Name: tool, Arguments: args, CallID: "c1"}
	v, err := eng.Would(context.Background(), agentturn.ToolCallInfo{
		Call: call, Args: json.RawMessage(args), Batch: []*openresponses.FunctionCall{call},
	})
	if err != nil {
		t.Fatal(err)
	}
	return v.Action, v.Reason + " [" + v.Subject + "]"
}

func bash(cmd string) string {
	b, _ := json.Marshal(map[string]string{"command": cmd})
	return string(b)
}

// testDir is the workspace the decisions are made in.
var testDir = func() string {
	d, err := os.MkdirTemp("", "dex-policy-")
	if err != nil {
		panic(err)
	}
	return d
}()

var defaults = config.PolicySettings{Builtin: true, Fallback: "ask"}

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
		{"skill", `{"name":"x"}`, agentturn.Allow},
		{"memory_search", `{"query":"x"}`, agentturn.Allow},
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
	user := func(r config.Rules) config.PolicySettings {
		s := defaults
		s.User = r
		return s
	}
	tests := []struct {
		name string
		s    config.PolicySettings
		tool string
		args string
		want agentturn.ToolAction
	}{
		{"allow a command", user(config.Rules{Allow: []string{"bash(make:*)"}}), "bash", bash("make check"), agentturn.Allow},
		{"allow does not reach other commands", user(config.Rules{Allow: []string{"bash(make:*)"}}), "bash", bash("make check && rm x"), agentturn.Defer},
		{"allow writes under a path", user(config.Rules{Allow: []string{"write(docs/**)"}}), "write", `{"path":"docs/a/b.md","content":""}`, agentturn.Allow},
		{"allow writes elsewhere still asks", user(config.Rules{Allow: []string{"write(docs/**)"}}), "write", `{"path":"src/a.go","content":""}`, agentturn.Defer},
		{"allow every write", user(config.Rules{Allow: []string{"write", "edit"}}), "edit", `{"path":"x","old_string":"a","new_string":"b"}`, agentturn.Allow},
		{"allow with the reference's names", user(config.Rules{Allow: []string{"Edit"}}), "write", `{"path":"x","content":""}`, agentturn.Allow},
		{"deny a built-in allowed command", user(config.Rules{Deny: []string{"bash(git log:*)"}}), "bash", bash("git log"), agentturn.Block},
		{"ask about a built-in allowed command", user(config.Rules{Ask: []string{"bash(git log:*)"}}), "bash", bash("git log"), agentturn.Defer},
		{"the recommended rule allows go test for a trusted repo", user(config.Rules{Allow: []string{"bash(go test:*)", "bash(go vet:*)", "bash(go build:*)"}}), "bash", bash("go test ./..."), agentturn.Allow},
		{"the recommended rule is still one simple command", user(config.Rules{Allow: []string{"bash(go test:*)"}}), "bash", bash("go test ./... && rm -rf x"), agentturn.Defer},
		{"deny beats allow", user(config.Rules{Allow: []string{"bash(rm:*)"}, Deny: []string{"bash(rm -rf:*)"}}), "bash", bash("rm -rf /"), agentturn.Block},
		{"deny reaches a subcommand", user(config.Rules{Deny: []string{"bash(rm:*)"}}), "bash", bash("git status && rm x"), agentturn.Block},
		{"a redirect never auto-runs, whatever the write rules say", user(config.Rules{Allow: []string{"write(out/**)"}}), "bash", bash("git log > out/log.txt"), agentturn.Defer},
		{"redirect elsewhere asks", user(config.Rules{Allow: []string{"write(out/**)"}}), "bash", bash("git log > ~/.bashrc"), agentturn.Defer},
		{"fallback allow", config.PolicySettings{Builtin: true, Fallback: "allow"}, "bash", bash("rm -rf x"), agentturn.Allow},
		{"fallback deny", config.PolicySettings{Builtin: true, Fallback: "deny"}, "bash", bash("rm -rf x"), agentturn.Block},
		{"fallback deny keeps the built-in allows", config.PolicySettings{Builtin: true, Fallback: "deny"}, "read", `{"path":"x"}`, agentturn.Allow},
		{"no built-in: even read asks", config.PolicySettings{Fallback: "ask"}, "read", `{"path":"x"}`, agentturn.Defer},
		{"no built-in: user allows read", config.PolicySettings{Fallback: "ask", User: config.Rules{Allow: []string{"Read"}}}, "grep", `{"pattern":"x"}`, agentturn.Allow},
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
	s.Project = config.Rules{
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
		// It can only make dex more careful.
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
	u.User = config.Rules{Allow: []string{"bash(curl:*)"}}
	if got, _ := decide(t, u, "bash", bash("curl https://x")); got != agentturn.Allow {
		t.Errorf("user allow = %v", got)
	}
}

func TestBadRulesAreErrorsNotSilence(t *testing.T) {
	// A specifier for a tool with no matcher does not build.
	s := defaults
	s.User = config.Rules{Allow: []string{"mcp__x(foo:*)"}}
	p, err := Build(s)
	if err == nil {
		_, err = agentpolicy.Build(p, Matchers(testDir, 0), Options()...)
	}
	if err == nil {
		t.Error("a specifier on a tool with no matcher should not build")
	}
	if _, err := Build(config.PolicySettings{Builtin: true, User: config.Rules{Allow: []string{"bash("}}}); err == nil || !strings.Contains(err.Error(), "policy allow") {
		t.Errorf("err = %v", err)
	}
}

func TestBuiltinAllowParses(t *testing.T) {
	if _, err := agentpolicy.ParseRules(BuiltinAllow); err != nil {
		t.Fatal(err)
	}
}

// Starting a sub-agent is allowed: it does nothing of itself, and what
// the sub-agent then does is decided call by call.
func TestStartingASubagentIsAllowedAndItsWritesAsk(t *testing.T) {
	shipped := config.PolicySettings{Builtin: true, Fallback: "ask"}
	for _, name := range []string{"task", "explore"} {
		if a, why := decide(t, shipped, name, `{"input":"x"}`); a != agentturn.Allow {
			t.Errorf("%s: %v (%s), want allowed", name, a, why)
		}
	}
	if a, _ := decide(t, shipped, "write", `{"path":"x","content":"y"}`); a != agentturn.Defer {
		t.Errorf("a write: %v, want asked", a)
	}
	// A user who drops the built-in list asks about starting one too.
	if a, _ := decide(t, config.PolicySettings{Builtin: false, Fallback: "ask"}, "task", `{"input":"x"}`); a != agentturn.Defer {
		t.Errorf("task without the built-in list: %v, want asked", a)
	}
}
