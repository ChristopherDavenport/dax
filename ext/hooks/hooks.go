// Package hooks is dax-hooks, the extension that runs the user's own
// PreToolUse and PostToolUse hooks under Claude Code's hook protocol:
// a command given the call as JSON on its standard input, whose exit
// status and output say what becomes of the call. It is built only from
// what package extension offers any extension.
//
// A hook is the user's: dax reads hooks from the user's config alone
// (a repository cannot add code that runs), and runs them on this
// machine whatever the workspace, in the directory the session started
// in, with the environment every child of the session gets, less its
// secrets (extension.Env.HostEnv).
//
// A PreToolUse hook takes part in the policy's decision of every call
// it matches, the sub-agents' included (extension.PolicyHooks): exit 2,
// or "permissionDecision": "deny", refuses the call with its stderr or
// its reason; "ask" asks about it; "allow" answers the policy's own ask
// as a person's yes would, only where that ask is the project's or the
// fallback's, never an extension's or the user's own ask rules, never a
// deny, never an A2A caller's call. Its additionalContext is the
// decision's note to the model. A PostToolUse hook runs after each call
// of the main agent it matches: exit 2, or "decision": "block", gives
// the model its stderr or reason after the result, and so does its
// additionalContext.
//
// A command given as a line runs as Claude Code runs it, through
// /bin/sh -c, so a pipe, && and ~ work, with CLAUDE_PROJECT_DIR set to
// the session's directory; the call itself reaches it on standard
// input alone. A PreToolUse hook that cannot answer (it does not
// start, the shell cannot run its program, a signal kills it, it runs
// past its timeout, or the call's arguments are not JSON) refuses the
// call: a guard fails closed. Another exit status than 0 and 2 is
// Claude Code's non-blocking error: the user is told and the call is
// decided as if the hook had said nothing. A PostToolUse hook's
// failure is the user's to see and leaves the result as it was. A run
// cancelled while a hook runs kills the hook's process group at once.
package hooks

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/extension"
)

// Name is the extension's name: dax-hooks.
const Name = "dax-hooks"

// By is the decider a hook's decision is recorded under.
const By = "hook"

// DefaultTimeout is a hook's time limit when it gives none.
const DefaultTimeout = 60 * time.Second

// maxOutput is the most of a hook's stdout or stderr read.
const maxOutput = 64 << 10

// Options are the hooks, by event.
type Options struct {
	PreToolUse  []Matcher
	PostToolUse []Matcher
}

// Matcher is the commands run for the calls whose tool's name Tools
// matches (nil matches every tool), in order.
type Matcher struct {
	Tools    *regexp.Regexp
	Commands []Command
}

// Command is one hook: Shell, a command line /bin/sh runs, as Claude
// Code runs a hook's command; or Argv, the program and its arguments,
// run with no shell. Timeout is its time limit (zero is
// DefaultTimeout).
type Command struct {
	Shell   string
	Argv    []string
	Timeout time.Duration
}

// argv is the process c starts.
func (c Command) argv() []string {
	if c.Shell != "" {
		return []string{"/bin/sh", "-c", c.Shell}
	}
	return c.Argv
}

// String names c in a line to the user.
func (c Command) String() string {
	if c.Shell != "" {
		return c.Shell
	}
	return strings.Join(c.Argv, " ")
}

// New is dax-hooks over o. With no hook it adds nothing to the session.
func New(o Options) extension.Extension {
	e := extension.Extension{Name: Name}
	if len(o.PreToolUse) > 0 {
		e.PolicyHooks = func(env extension.Env) (extension.PolicyHooks, error) {
			r := &runner{env: env, matchers: o.PreToolUse, memo: map[string]preResult{}}
			return extension.PolicyHooks{Decide: r.decide, Answer: r.answer}, nil
		}
	}
	if len(o.PostToolUse) > 0 {
		e.Kit = func(env extension.Env) ([]agentkit.Option, error) {
			r := &runner{env: env, matchers: o.PostToolUse}
			return []agentkit.Option{agentkit.WithAfterToolCall(r.after)}, nil
		}
	}
	return e
}

// input is what a hook reads on its standard input: Claude Code's
// fields, for the ones dax has. cwd is the root of the workspace the
// tools act in.
type input struct {
	Event        string          `json:"hook_event_name"`
	CWD          string          `json:"cwd"`
	Tool         string          `json:"tool_name"`
	ToolInput    json.RawMessage `json:"tool_input"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	ToolResponse *response       `json:"tool_response,omitempty"`
}

// response is a call's result as a PostToolUse hook reads it.
type response struct {
	Output string `json:"output"`
	Error  string `json:"error,omitempty"`
}

// output is what a hook may print on its standard output.
type output struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
	Specific struct {
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason"`
		AdditionalContext        string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

// result is one hook's run: blocked (exit 2) with its stderr, or its
// output read; or failed, when it could say nothing, with fatal set
// when it gave no answer at all (it did not start or run to its end),
// which refuses the call it guards.
type result struct {
	blocked bool
	stderr  string
	out     output
	failed  string
	fatal   bool
}

// preResult is the PreToolUse hooks' word on a call, folded: said is
// false when no hook allowed, asked about or refused it.
type preResult struct {
	said   bool
	action agentturn.ToolAction
	reason string
	note   string
}

type runner struct {
	env      extension.Env
	matchers []Matcher

	// memo holds the hooks' word on a call by its ID and arguments, so
	// the engine's Decide and Answer hooks, and a decision it asks
	// again for a sibling or a probe, run each hook once per call.
	mu    sync.Mutex
	memo  map[string]preResult
	order []string
}

// memoSize is how many calls' words a runner keeps.
const memoSize = 256

func (r *runner) commands(tool string) []Command {
	var out []Command
	for _, m := range r.matchers {
		if m.Tools == nil || m.Tools.MatchString(tool) {
			out = append(out, m.Commands...)
		}
	}
	return out
}

// pre is the PreToolUse hooks' word on the call info carries, folded
// deny over ask over allow.
func (r *runner) pre(ctx context.Context, info agentturn.ToolCallInfo) preResult {
	if info.Call == nil {
		return preResult{}
	}
	cmds := r.commands(info.Call.Name)
	if len(cmds) == 0 {
		return preResult{}
	}
	args := argsOf(info.Args, info.Call.Arguments)
	if !json.Valid(args) {
		r.env.Log("[PreToolUse hooks: the arguments of %s are not JSON; the call is refused]", info.Call.Name)
		return preResult{said: true, action: agentturn.Block, reason: "the call's arguments are not JSON, which a PreToolUse hook cannot read, so the call is refused"}
	}
	sum := sha256.Sum256(args)
	key := info.Call.CallID + "\x00" + info.Call.Name + "\x00" + string(sum[:])
	r.mu.Lock()
	if p, ok := r.memo[key]; ok {
		r.mu.Unlock()
		return p
	}
	r.mu.Unlock()
	in := input{Event: "PreToolUse", CWD: r.root(), Tool: info.Call.Name, ToolInput: args, ToolUseID: info.Call.CallID}
	var p preResult
	var notes []string
	for _, c := range cmds {
		res := r.run(ctx, c, in)
		if res.fatal {
			// A guard that gave no answer refuses: failing open would
			// let through the calls it is there to stop.
			p.said, p.action, p.reason = true, agentturn.Block, "a PreToolUse hook ("+c.String()+") could not answer ("+res.failed+"), so the call is refused"
			break
		}
		if res.failed != "" {
			continue
		}
		if n := res.out.Specific.AdditionalContext; n != "" {
			notes = append(notes, n)
		}
		said, action, reason := true, agentturn.Allow, ""
		switch {
		case res.blocked:
			action, reason = agentturn.Block, res.stderr
		case res.out.Specific.PermissionDecision == "deny":
			action, reason = agentturn.Block, res.out.Specific.PermissionDecisionReason
		case res.out.Specific.PermissionDecision == "ask":
			action, reason = agentturn.Defer, res.out.Specific.PermissionDecisionReason
		case res.out.Specific.PermissionDecision == "allow":
			action, reason = agentturn.Allow, res.out.Specific.PermissionDecisionReason
		default:
			said = false
		}
		if said && (!p.said || rank(action) > rank(p.action)) {
			p.said, p.action, p.reason = true, action, reason
		}
	}
	if p.said && p.action == agentturn.Block && p.reason == "" {
		p.reason = "a PreToolUse hook refused this call"
	}
	if p.said && p.action == agentturn.Defer && p.reason == "" {
		p.reason = "a PreToolUse hook asks about this call"
	}
	p.note = strings.Join(notes, "\n")
	if ctx.Err() != nil {
		// A word cut short by a cancelled run is not the hooks' word on
		// the call, which may be decided again.
		return p
	}
	r.mu.Lock()
	if _, ok := r.memo[key]; !ok {
		r.order = append(r.order, key)
		if len(r.order) > memoSize {
			delete(r.memo, r.order[0])
			r.order = r.order[1:]
		}
	}
	r.memo[key] = p
	r.mu.Unlock()
	return p
}

func rank(a agentturn.ToolAction) int {
	switch a {
	case agentturn.Block:
		return 3
	case agentturn.Defer:
		return 2
	case agentturn.Allow:
		return 1
	}
	return 0
}

// decide is the hooks' refusal or question, folded with the policy's.
func (r *runner) decide(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
	p := r.pre(ctx, info)
	switch {
	case p.said && (p.action == agentturn.Block || p.action == agentturn.Defer):
		return &agentturn.ToolDecision{Action: p.action, Reason: p.reason, Note: p.note, By: By}, nil
	}
	if p.note != "" {
		// A note alone, on a call the hooks did not refuse or ask
		// about, rides the policy's decision as an allow does nothing.
		return &agentturn.ToolDecision{Action: agentturn.Allow, Note: p.note, By: By}, nil
	}
	return nil, nil
}

// answer is the hooks' allow, which may answer the policy's own ask.
func (r *runner) answer(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
	p := r.pre(ctx, info)
	if !p.said || p.action != agentturn.Allow {
		return nil, nil
	}
	reason := p.reason
	if reason == "" {
		reason = "a PreToolUse hook allowed this call"
	}
	return &agentturn.ToolDecision{Action: agentturn.Allow, Reason: reason, Note: p.note, By: By}, nil
}

// after runs the PostToolUse hooks of a call of the main agent, and
// gives the model what they say after the result.
func (r *runner) after(ctx context.Context, info agentturn.ToolResultInfo) (*agentturn.ToolOverride, error) {
	if info.Call == nil {
		return nil, nil
	}
	cmds := r.commands(info.Call.Name)
	if len(cmds) == 0 {
		return nil, nil
	}
	args := argsOf(info.Args, info.Call.Arguments)
	if !json.Valid(args) {
		r.env.Log("[PostToolUse hooks: the arguments of %s are not JSON; the hooks are not run]", info.Call.Name)
		return nil, nil
	}
	resp := &response{Output: info.Result.Output.String()}
	if info.Err != nil {
		resp.Error = info.Err.Error()
	}
	in := input{Event: "PostToolUse", CWD: r.root(), Tool: info.Call.Name, ToolInput: args, ToolUseID: info.Call.CallID, ToolResponse: resp}
	var said []string
	for _, c := range cmds {
		res := r.run(ctx, c, in)
		if res.failed != "" {
			continue
		}
		switch {
		case res.blocked && res.stderr != "":
			said = append(said, res.stderr)
		case res.out.Decision == "block" && res.out.Reason != "":
			said = append(said, res.out.Reason)
		}
		if n := res.out.Specific.AdditionalContext; n != "" {
			said = append(said, n)
		}
	}
	if len(said) == 0 {
		return nil, nil
	}
	return &agentturn.ToolOverride{Result: withNote(info.Result, "[PostToolUse hook]\n"+strings.Join(said, "\n")), Err: info.Err}, nil
}

// withNote is res with text after its output.
func withNote(res agenttool.Result, text string) agenttool.Result {
	out := res
	if out.Output.Parts != nil {
		out.Output.Parts = append(append(openresponses.Contents(nil), out.Output.Parts...), &openresponses.InputText{Text: text})
		return out
	}
	if out.Output.Text != "" {
		text = out.Output.Text + "\n\n" + text
	}
	out.Output.Text = text
	return out
}

// root is the workspace's root, the cwd a hook is told.
func (r *runner) root() string {
	if ws := r.env.ToolEnv().Workspace; ws != nil {
		return ws.Root()
	}
	return r.env.Dir()
}

// argsOf is a call's arguments as a hook reads them: the decoded ones,
// else the call's, and {} for none.
func argsOf(decoded json.RawMessage, raw string) json.RawMessage {
	args := decoded
	if args == nil {
		args = json.RawMessage(raw)
	}
	if len(bytes.TrimSpace(args)) == 0 {
		return json.RawMessage(`{}`)
	}
	return args
}

// run runs one hook with in on its standard input, in its own process
// group, which a timeout or the run's cancellation kills whole. A
// result that failed says why, and the user is told.
func (r *runner) run(ctx context.Context, c Command, in input) result {
	argv := c.argv()
	what := "the call goes on as it was"
	if in.Event == "PreToolUse" {
		what = "the call is refused"
	}
	fail := func(fatal bool, why string) result {
		if fatal || in.Event != "PreToolUse" {
			r.env.Log("[%s hook %s: %s; %s]", in.Event, c, why, what)
		} else {
			r.env.Log("[%s hook %s: %s]", in.Event, c, why)
		}
		return result{failed: why, fatal: fatal}
	}
	if len(argv) == 0 || argv[0] == "" {
		return fail(true, "no program")
	}
	data, err := json.Marshal(in)
	if err != nil {
		return fail(true, "its input: "+err.Error())
	}
	limit := c.Timeout
	if limit <= 0 {
		limit = DefaultTimeout
	}
	hctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	cmd := exec.CommandContext(hctx, argv[0], argv[1:]...)
	cmd.Dir = r.env.Dir()
	cmd.Env = append(slices.Clone(r.env.HostEnv()), "CLAUDE_PROJECT_DIR="+r.env.Dir())
	cmd.Stdin = bytes.NewReader(data)
	var stdout, stderr capped
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = time.Second
	ownGroup(cmd)
	err = cmd.Run()
	errText := strings.TrimSpace(stderr.String())
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return fail(true, "the run was cancelled")
	case hctx.Err() != nil:
		return fail(true, "no answer within "+limit.String())
	case errors.As(err, &exit) && exit.ExitCode() == 2:
		return result{blocked: true, stderr: errText}
	case errors.As(err, &exit) && exit.ExitCode() == -1:
		return fail(true, "killed: "+exit.Error())
	case errors.As(err, &exit) && (exit.ExitCode() == 126 || exit.ExitCode() == 127):
		// The shell's word for a program it could not find or run.
		return fail(true, joinText("could not run its program ("+exit.Error()+")", errText))
	case errors.As(err, &exit):
		return fail(false, joinText(exit.Error(), errText))
	case err != nil:
		return fail(true, "did not start: "+err.Error())
	}
	var res result
	if t := bytes.TrimSpace(stdout.Bytes()); len(t) > 0 && t[0] == '{' {
		if err := json.Unmarshal(t, &res.out); err != nil {
			r.env.Log("[%s hook %s: its output is not the hook's JSON: %v]", in.Event, c, err)
		}
	}
	return res
}

// joinText is a and, when there is one, b after a colon.
func joinText(a, b string) string {
	if b == "" {
		return a
	}
	return a + ": " + b
}

// capped is a buffer that keeps the first maxOutput bytes and drops the
// rest.
type capped struct{ bytes.Buffer }

func (c *capped) Write(p []byte) (int, error) {
	if room := maxOutput - c.Len(); room > 0 {
		c.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

var _ io.Writer = (*capped)(nil)
