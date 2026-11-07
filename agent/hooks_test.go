package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/ext/hooks"
	"github.com/ChristopherDavenport/dax/policy"
)

// hookScript writes an executable shell script and returns its argv.
func hookScript(t *testing.T, body string) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hook.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{path}
}

// withHooks adds dax-hooks to o with one PreToolUse hook for the tools
// tools matches.
func withHooks(o Options, tools string, pre, post []string) Options {
	var h hooks.Options
	if pre != nil {
		h.PreToolUse = []hooks.Matcher{{Tools: regexp.MustCompile(`^(?:` + tools + `)$`), Commands: []hooks.Command{{Argv: pre}}}}
	}
	if post != nil {
		h.PostToolUse = []hooks.Matcher{{Tools: regexp.MustCompile(`^(?:` + tools + `)$`), Commands: []hooks.Command{{Argv: post}}}}
	}
	o.Extensions = append(slices.Clone(o.Extensions), hooks.New(h))
	return o
}

// A PreToolUse hook that exits 2 refuses the call with its stderr as
// the reason, before anyone is asked, and runs without the session's
// secrets in its environment (#148).
func TestAHookThatExitsTwoRefusesTheCallWithItsStderr(t *testing.T) {
	ctx := context.Background()
	t.Setenv("DAX_HOOKTEST_TOKEN", "s3cret")
	envFile := filepath.Join(t.TempDir(), "env")
	hook := hookScript(t, `env > '`+envFile+`'
if grep -q 'push --force'; then echo 'no force pushes here' >&2; exit 2; fi
exit 0
`)
	o := withHooks(options(t, &scripted{calls: [][2]string{{"bash", `{"command":"git push --force"}`}}}), "bash", hook, nil)
	o.Policy = confirmPolicy(t)
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var asked []string
	if _, err := promptOn(ctx, s, "go", asker(&asked)); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 0 {
		t.Errorf("asked about %v; the hook's refusal comes first", asked)
	}
	var outs []string
	for _, it := range s.Agent().State().Transcript {
		if out, ok := it.(*openresponses.FunctionCallOutput); ok {
			outs = append(outs, out.Output.String())
		}
	}
	if len(outs) != 1 || !strings.Contains(outs[0], "no force pushes here") {
		t.Errorf("the output: %q, want the hook's stderr", outs)
	}
	env, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(env), "s3cret") {
		t.Error("the hook was given a secret variable")
	}
}

// A hook's allow answers the policy's fallback ask, never a deny, and
// never an A2A caller's call (#148).
func TestAHooksAllowLiftsTheFallbackAskButNotADeny(t *testing.T) {
	ctx := context.Background()
	hook := hookScript(t, `cat > /dev/null
echo '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","permissionDecisionReason":"my hook says yes"}}'
`)
	o := withHooks(options(t, &scripted{}), "bash", hook, nil)
	o.Policy = confirmPolicy(t)
	o.Policy.User = policy.Rules{Deny: []string{"bash(touch denied*)"}}
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	bash, ok := s.Kit.LookupTool("bash")
	if !ok {
		t.Fatal("no bash")
	}
	info := func(id, cmd string) agentturn.ToolCallInfo {
		args := `{"command":"` + cmd + `"}`
		call := &openresponses.FunctionCall{Name: "bash", Arguments: args, CallID: id}
		return agentturn.ToolCallInfo{Call: call, Tool: bash, Args: json.RawMessage(args), Batch: []*openresponses.FunctionCall{call}}
	}
	eng := s.Kit.Engine()
	// Without the hook, the fallback asks about it.
	if v, err := eng.Would(ctx, info("c1", "touch lifted")); err != nil || v.Action != agentturn.Allow || v.Reason != "my hook says yes" {
		t.Errorf("an asked call: %v %q (%v), want allowed by the hook", v.Action, v.Reason, err)
	}
	if v, err := eng.Would(ctx, info("c2", "touch denied")); err != nil || v.Action != agentturn.Block {
		t.Errorf("a denied call: %v %q (%v), want refused", v.Action, v.Reason, err)
	}
	peer := context.WithValue(ctx, peerKey{}, "A2A caller alice")
	if v, err := eng.Would(peer, info("c3", "touch caller")); err != nil || v.Action != agentturn.Defer {
		t.Errorf("an A2A caller's call: %v %q (%v), want still asked", v.Action, v.Reason, err)
	}
}

// A PostToolUse hook's word reaches the model after the result.
func TestAPostToolUseHookSpeaksAfterTheResult(t *testing.T) {
	ctx := context.Background()
	post := hookScript(t, `cat > /dev/null
echo '{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":"run gofmt next"}}'
`)
	o := withHooks(options(t, &scripted{calls: [][2]string{{"ls", `{}`}}}), "ls", nil, post)
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := promptOn(ctx, s, "go", nil); err != nil {
		t.Fatal(err)
	}
	var outs []string
	for _, it := range s.Agent().State().Transcript {
		if out, ok := it.(*openresponses.FunctionCallOutput); ok {
			outs = append(outs, out.Output.String())
		}
	}
	if len(outs) != 1 || !strings.Contains(outs[0], "run gofmt next") || !strings.Contains(outs[0], "AGENTS.md") {
		t.Errorf("the output: %q, want ls's listing and the hook's word after it", outs)
	}
}

// A hook's allow answers the project's ask, which only tightens what
// the user allows, but never the user's own ask rule (rank 2): what the
// user asked to be asked about is still asked (#148, review 13).
func TestAHooksAllowLiftsTheProjectsAskButNotTheUsers(t *testing.T) {
	ctx := context.Background()
	hook := hookScript(t, `cat > /dev/null
echo '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","permissionDecisionReason":"my hook says yes"}}'
`)
	o := withHooks(options(t, &scripted{}), "bash", hook, nil)
	o.Policy = confirmPolicy(t)
	o.Policy.User = policy.Rules{Ask: []string{"bash(touch mine*)"}}
	o.Policy.Project = policy.Rules{Ask: []string{"bash(touch theirs*)"}}
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	bash, ok := s.Kit.LookupTool("bash")
	if !ok {
		t.Fatal("no bash")
	}
	info := func(id, cmd string) agentturn.ToolCallInfo {
		args := `{"command":"` + cmd + `"}`
		call := &openresponses.FunctionCall{Name: "bash", Arguments: args, CallID: id}
		return agentturn.ToolCallInfo{Call: call, Tool: bash, Args: json.RawMessage(args), Batch: []*openresponses.FunctionCall{call}}
	}
	eng := s.Kit.Engine()
	if v, err := eng.Would(ctx, info("c1", "touch theirs")); err != nil || v.Action != agentturn.Allow || v.Reason != "my hook says yes" {
		t.Errorf("the project's ask: %v %q (%v), want lifted by the hook", v.Action, v.Reason, err)
	}
	if v, err := eng.Would(ctx, info("c2", "touch mine")); err != nil || v.Action != agentturn.Defer {
		t.Errorf("the user's own ask: %v %q (%v), want still asked", v.Action, v.Reason, err)
	}
}
