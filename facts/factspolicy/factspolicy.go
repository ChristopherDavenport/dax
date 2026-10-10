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
// tool its extension does not have and is not held to: no rule names
// it, so the call falls to the policy's default, which asks, as a call
// of the claiming extension's own tool with no rule would. One
// extension's claim never borrows another's allow (or the user's rules
// for another's tool).
const outside = "dax:outside-its-extension"

// Subjects is the subjects splitter of a tool that claims: each call
// its facts name is a subject (see SubjectsOf; own are the names of the
// tool's extension, held the tools whose asks and denies the tool's
// calls are held to), and a claim with no calls is the call itself. An
// error is the claim's, which blocks the call as an unreadable one
// does. It is nil for a tool that makes no claim, which agentpolicy
// reads as one subject per call.
//
// The claim is asked under the context of the decision that reads the
// subjects (agentpolicy passes Decide's, Would's, or, when the batch
// hold reads a sibling, the held decision's), so a claim that asks a
// remote executor is cancelled with the decision and keeps its
// deadline; a cancelled context is the claim's error and blocks the
// call. The session's tools are the executor's stand-ins, which answer
// every reading made under a decision's context from its model
// response's one reading (internal/executor's Set.PinBatch), so the
// engine's several readings of one call, and its readings of the
// call's siblings, are one; a call no batch holds is read under the
// context given here.
func Subjects(t agenttool.Tool, own, held []string) agentpolicy.Subjects {
	if !agenttool.IsFactual(t) {
		return nil
	}
	return func(ctx context.Context, args json.RawMessage) ([]agentpolicy.Subject, error) {
		f, _, err := agenttool.FactsOf(ctx, t, args)
		if err != nil {
			return nil, err
		}
		return SubjectsOf(f.Calls, args, own, held), nil
	}
}

// SubjectsOf is calls as agentpolicy's subjects; nil calls are the call
// itself, args. A call may name the claiming tool (Tool empty) or
// another of its extension's tools, own (bash's claim of a read of what
// cat reads). One that names a tool in held, another extension's tool
// the claiming tool's calls are held to (extension.Extension.HeldTo),
// is a constraint (agentpolicy's Subject.Constrain): that tool's ask
// and deny rules can make the call ask or refuse it, and its allow
// rules never allow it. One that names any other tool is decided as a
// tool no rule names, so it asks, whatever the rules for the tool it
// named say.
func SubjectsOf(calls []agenttool.FactCall, args json.RawMessage, own, held []string) []agentpolicy.Subject {
	if calls == nil {
		return []agentpolicy.Subject{{Args: args}}
	}
	out := make([]agentpolicy.Subject, 0, len(calls))
	for _, c := range calls {
		out = append(out, confine(agentpolicy.Subject{Args: c.Args, Tool: c.Tool, Text: c.Text}, own, held))
	}
	return out
}

// confine is s as its extension may have it decided (see SubjectsOf):
// s itself when it names the called tool or one of own, a constraint
// when it names a tool in held, and otherwise a subject of a tool no
// rule names, which can neither borrow that tool's rules nor be a
// constraint that nothing fires for.
func confine(s agentpolicy.Subject, own, held []string) agentpolicy.Subject {
	switch {
	case s.Tool == "" || slices.Contains(own, s.Tool):
	case slices.Contains(held, s.Tool):
		s.Constrain = true
	default:
		s.Tool, s.Constrain = outside, false
	}
	return s
}

// Confine is an extension's matcher's own splitter, s, held to what a
// claim of its tool may name (see SubjectsOf): a subject that names a
// tool not in own, another extension's, is decided as a tool no rule
// names, so it asks, and a matcher can no more borrow another
// extension's rules than a claim can. A matcher's subjects are never
// held to another tool (extension.Extension.HeldTo is the claim's).
// Nil for a nil s.
func Confine(s agentpolicy.Subjects, own []string) agentpolicy.Subjects {
	if s == nil {
		return nil
	}
	return func(ctx context.Context, args json.RawMessage) ([]agentpolicy.Subject, error) {
		subs, err := s(ctx, args)
		if err != nil {
			return nil, err
		}
		out := make([]agentpolicy.Subject, len(subs))
		for i, sub := range subs {
			out[i] = confine(sub, own, nil)
		}
		return out, nil
	}
}

// Matchers are match, an extension's matchers, with the subjects of
// every tool in tools, the extension's, that claims taken from its
// claim; own are the names the extension has (its tools, the names it
// owns and its aliases), the tools its claims and its matchers'
// subjects may name (SubjectsOf, Confine), and held the other tools
// each of its tools' claims may name as constraints, by tool
// (extension.Extension.HeldTo). A matcher that brings subjects of
// its own for a claiming tool is an error: the tool's claim is what its
// calls touch, and a second reading of them would be a second answer.
func Matchers(tools []agenttool.Tool, own []string, held map[string][]string, match map[string]agentpolicy.ToolMatcher) (map[string]agentpolicy.ToolMatcher, error) {
	out := make(map[string]agentpolicy.ToolMatcher, len(match))
	for name, m := range match {
		m.Subjects = Confine(m.Subjects, own)
		out[name] = m
	}
	for _, t := range tools {
		s := Subjects(t, own, held[t.Name()])
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
	return HookFor(func(name string) (agenttool.Tool, bool) {
		t, ok := byName[name]
		return t, ok
	})
}

// HookFor is Hook over the tools lookup finds by name at each call,
// for a tool that exists only once the session's kit is built (one an
// extension's Kit adds, skill say): a call of a tool lookup does not
// find, or of one that makes no claim, is left alone.
func HookFor(lookup func(name string) (agenttool.Tool, bool)) func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
	return func(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
		if info.Call == nil {
			return nil, nil
		}
		t, ok := lookup(info.Call.Name)
		if !ok || !agenttool.IsFactual(t) {
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
