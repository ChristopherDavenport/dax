// Package factspolicy is the policy's side of agenttool's facts claim
// (agenttool.Factual): the subjects agentpolicy decides a call on, taken
// from what the tool says the call would touch, and the rewrite a tool
// asks for, applied as a hook folded under the policy's verdict. It is
// where the claim meets agentpolicy, which agenttool does not import.
package factspolicy

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
)

// outside is the tool a claimed call is decided as when it names a
// tool its extension does not have: no rule names it, so the call
// falls to the policy's default, which asks, as a call of the claiming
// extension's own tool with no rule would. One extension's claim never
// borrows another's allow (or the user's rules for another's tool).
const outside = "dax:outside-its-extension"

// Subjects is the subjects splitter of a tool that claims: each call
// its facts name is a subject (see SubjectsOf; own are the names of the
// tool's extension), and a claim with no calls is the call itself. An
// error is the claim's, which blocks the call as an unreadable one
// does. It is nil for a tool that makes no claim, which agentpolicy
// reads as one subject per call.
//
// agentpolicy.Subjects takes no context, so the claim is asked under
// context.Background(). The session's tools are the executor's
// stand-ins, which answer every reading made while a decision about the
// call is in flight from one reading taken under the call's context
// (internal/executor's Set.Pin), so the engine's several readings of
// one call are one. A remote executor still wants a context and a
// deadline here: agentpolicy's subjects with a context, step 3 of the
// plan.
func Subjects(t agenttool.Tool, own []string) agentpolicy.Subjects {
	if !agenttool.IsFactual(t) {
		return nil
	}
	return func(args json.RawMessage) ([]agentpolicy.Subject, error) {
		f, _, err := agenttool.FactsOf(context.Background(), t, args)
		if err != nil {
			return nil, err
		}
		return SubjectsOf(f.Calls, args, own), nil
	}
}

// SubjectsOf is calls as agentpolicy's subjects; nil calls are the call
// itself, args. A call may name the claiming tool (Tool empty) or
// another of its extension's tools, own (bash's claim of a read of what
// cat reads); one that names any other tool is decided as a tool no
// rule names, so it asks, whatever the rules for the tool it named say.
func SubjectsOf(calls []agenttool.FactCall, args json.RawMessage, own []string) []agentpolicy.Subject {
	if calls == nil {
		return []agentpolicy.Subject{{Args: args}}
	}
	out := make([]agentpolicy.Subject, 0, len(calls))
	for _, c := range calls {
		tool := c.Tool
		if tool != "" && !slices.Contains(own, tool) {
			tool = outside
		}
		out = append(out, agentpolicy.Subject{Args: c.Args, Tool: tool, Text: c.Text})
	}
	return out
}

// Matchers are match, an extension's matchers, with the subjects of
// every tool in tools, the extension's, that claims taken from its
// claim; own are the names the extension has (its tools, the names it
// owns and its aliases), the tools its claims may name (SubjectsOf). A
// matcher that brings subjects of its own for a claiming tool is an
// error: the tool's claim is what its calls touch, and a second
// reading of them would be a second answer.
func Matchers(tools []agenttool.Tool, own []string, match map[string]agentpolicy.ToolMatcher) (map[string]agentpolicy.ToolMatcher, error) {
	out := make(map[string]agentpolicy.ToolMatcher, len(match))
	for name, m := range match {
		out[name] = m
	}
	for _, t := range tools {
		s := Subjects(t, own)
		if s == nil {
			continue
		}
		m := out[t.Name()]
		if m.Subjects != nil {
			return nil, fmt.Errorf("matcher for %q: the tool's facts claim gives its subjects", t.Name())
		}
		m.Subjects = s
		out[t.Name()] = m
	}
	return out, nil
}

// Hook is the BeforeToolCall that applies the rewrite a claiming
// tool's facts ask for: Allow with the rewritten arguments. Folded
// under the policy's verdict, deny over ask over allow, its allow never
// lets a call run that the policy asks about; the rewrite stays on the
// decision, so a call a person approves runs rewritten too (a stamp the
// model forged replaced or taken off), the main agent's through
// agentturn and a sub-agent's through the session's check of its
// calls. A claim that fails blocks the call, as the policy's subjects
// do, so a call nothing can be said about never runs on the model's own
// arguments. It decides nothing else. Nil when no tool claims.
func Hook(tools []agenttool.Tool) func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
	byName := map[string]agenttool.Tool{}
	for _, t := range tools {
		if agenttool.IsFactual(t) {
			byName[t.Name()] = t
		}
	}
	if len(byName) == 0 {
		return nil
	}
	return func(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
		if info.Call == nil {
			return nil, nil
		}
		t, ok := byName[info.Call.Name]
		if !ok {
			return nil, nil
		}
		f, _, err := agenttool.FactsOf(ctx, t, info.Args)
		if err != nil {
			return &agentturn.ToolDecision{Action: agentturn.Block, Reason: "what this call would touch cannot be read: " + err.Error(), By: agentpolicy.ByPolicy}, nil
		}
		if f.Rewrite == nil {
			return nil, nil
		}
		return &agentturn.ToolDecision{Action: agentturn.Allow, Args: f.Rewrite}, nil
	}
}
