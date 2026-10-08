package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/internal/prompt"
	"github.com/ChristopherDavenport/dax/policy"
)

// assembly is a session's extensions, built: their tools in order, and
// what the policy and the sub-agents need of them.
type assembly struct {
	tools, readOnly []agenttool.Tool
	matchers        map[string]agentpolicy.ToolMatcher
	aliases         map[string][]string
	shipped         []policy.Shipped
	hooks           []func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error)
}

// mcpPrefixLower is the prefix of every MCP server's tools, which no
// extension may take, so the user's mcp__* rules reach only servers.
const mcpPrefixLower = "mcp__"

// build builds the extensions' tools over env and checks what each
// claims: its name, its tools, the names it owns and its aliases are
// unique across the session without regard to case; a read-only tool,
// a matcher and an alias name only the extension's own tools. On an
// error the tools built so far are closed.
func build(exts []extension.Extension, env extension.ToolEnv) (a *assembly, err error) {
	a = &assembly{matchers: map[string]agentpolicy.ToolMatcher{}, aliases: map[string][]string{}}
	defer func() {
		if err != nil {
			agenttool.Set(a.tools).Close()
			a = nil
		}
	}()
	extNames := map[string]bool{}
	claimed := map[string]string{} // lower-case name -> the extension that has it
	claim := func(ext, name, what string) error {
		low := strings.ToLower(name)
		switch {
		case name == "":
			return fmt.Errorf("extension %s: a %s with no name", ext, what)
		case strings.HasPrefix(low, mcpPrefixLower):
			return fmt.Errorf("extension %s: %s %q: mcp__ names an MCP server's tools", ext, what, name)
		case claimed[low] != "" && !(what == "alias" && claimed[low] == ext):
			// An alias may differ from its own extension's tool only in
			// case, as dax-coding's Bash does from bash; it may not take
			// another extension's name.
			return fmt.Errorf("extension %s: %s %q: extension %s has that name", ext, what, name, claimed[low])
		}
		claimed[low] = ext
		return nil
	}
	for _, e := range exts {
		if e.Name == "" {
			return a, errors.New("an extension with no name")
		}
		if extNames[e.Name] {
			return a, fmt.Errorf("two extensions named %s", e.Name)
		}
		extNames[e.Name] = true
		own := map[string]bool{}
		var tools []agenttool.Tool
		if e.Tools != nil {
			tools = e.Tools(env)
		}
		for _, t := range tools {
			if t == nil {
				return a, fmt.Errorf("extension %s: a nil tool", e.Name)
			}
			a.tools = append(a.tools, t)
			if err := claim(e.Name, t.Name(), "tool"); err != nil {
				return a, err
			}
			own[t.Name()] = true
		}
		for _, n := range e.ReadOnly {
			i := slices.IndexFunc(tools, func(t agenttool.Tool) bool { return t.Name() == n })
			if i < 0 {
				return a, fmt.Errorf("extension %s: read-only %q is not one of its tools", e.Name, n)
			}
			// The annotations are hints, never enough to allow; they
			// may make a decision stricter, as refusing this one does.
			if an := agenttool.AnnotationsOf(tools[i]); !an.ReadOnly && an.Destructive {
				return a, fmt.Errorf("extension %s: %q is read-only but annotated destructive", e.Name, n)
			}
			a.readOnly = append(a.readOnly, tools[i])
		}
		for _, n := range e.Owns {
			if err := claim(e.Name, n, "tool"); err != nil {
				return a, err
			}
			own[n] = true
		}
		ruleNames := slices.Collect(maps.Keys(own))
		for alias, targets := range e.Aliases {
			if err := claim(e.Name, alias, "alias"); err != nil {
				return a, err
			}
			for _, t := range targets {
				if !own[t] {
					return a, fmt.Errorf("extension %s: alias %s names %q, which is not one of its tools", e.Name, alias, t)
				}
			}
			a.aliases[alias] = targets
			ruleNames = append(ruleNames, alias)
		}
		var ms map[string]agentpolicy.ToolMatcher
		if e.Matchers != nil {
			ms = e.Matchers(env)
		}
		for name, m := range ms {
			if !own[name] {
				return a, fmt.Errorf("extension %s: matcher for %q, which is not one of its tools", e.Name, name)
			}
			a.matchers[name] = m
		}
		for _, n := range e.Lifts {
			if !own[n] {
				return a, fmt.Errorf("extension %s: lifts %q, which is not one of its tools", e.Name, n)
			}
		}
		if p := e.Policy; len(p.Allow)+len(p.Ask)+len(p.Deny)+len(e.Lifts) > 0 {
			slices.Sort(ruleNames)
			a.shipped = append(a.shipped, policy.Shipped{Name: e.Name, Rules: p, Owns: ruleNames, Lifts: e.Lifts})
		}
		if e.BeforeToolCall != nil {
			if h := e.BeforeToolCall(env); h != nil {
				a.hooks = append(a.hooks, h)
			}
		}
	}
	return a, nil
}

// hook is the extensions' BeforeToolCall hooks as one, nil for none.
func (a *assembly) hook() func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
	if len(a.hooks) == 0 {
		return nil
	}
	return agentturn.ChainBeforeToolCall(a.hooks...)
}

// env is the session as the extensions' kit options see it.
type sessionEnv struct {
	s        *Session
	a        *assembly
	te       extension.ToolEnv
	agentsMD string
	eng      *atomic.Pointer[agentpolicy.Engine]
}

var _ extension.Env = (*sessionEnv)(nil)

func (e *sessionEnv) ToolEnv() extension.ToolEnv           { return e.te }
func (e *sessionEnv) Name() string                         { return e.s.opts.Name }
func (e *sessionEnv) Dir() string                          { return e.s.opts.Dir }
func (e *sessionEnv) UserDir() string                      { return e.s.opts.UserDir }
func (e *sessionEnv) Tools() []agenttool.Tool              { return slices.Clone(e.a.tools) }
func (e *sessionEnv) ReadOnlyTools() []agenttool.Tool      { return slices.Clone(e.a.readOnly) }
func (e *sessionEnv) AgentsMD() string                     { return e.agentsMD }
func (e *sessionEnv) Model() openresponses.Streamer        { return e.s.opts.Streamer }
func (e *sessionEnv) Omit(o agentkit.Omission)             { e.s.refused = append(e.s.refused, o) }
func (e *sessionEnv) Log(format string, args ...any)       { e.s.opts.log(format, args...) }
func (e *sessionEnv) SystemPrompt(except ...string) string { return e.s.systemPrompt(except...) }

func (e *sessionEnv) Live() (string, bool) {
	think, main := e.s.now()
	return main, think
}

func (e *sessionEnv) Reasoning(ctx context.Context, model string, think bool) openresponses.ReasoningConfig {
	o := e.s.opts
	o.Think = think
	return o.reasoningFor(ctx, model)
}

func (e *sessionEnv) ChildPolicy(name string) func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
	if e.s.opts.Policy == nil {
		return nil
	}
	return e.s.opts.childPolicy(name, e.eng, e.a.hook())
}

// systemPrompt is the main agent's part of the system prompt: dax's
// role line, the instructions of every extension but those named in
// except, in order, the user's own, and the workspace's root as the
// working directory.
func (s *Session) systemPrompt(except ...string) string {
	o := s.opts
	var extra []string
	for _, e := range o.Extensions {
		if !slices.Contains(except, e.Name) {
			extra = append(extra, e.Instructions)
		}
	}
	return prompt.Build(o.Name, s.ws.Root(), append(extra, o.Instructions)...)
}
