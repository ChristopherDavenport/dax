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
	"github.com/ChristopherDavenport/dax/facts/factspolicy"
	"github.com/ChristopherDavenport/dax/internal/executor"
	"github.com/ChristopherDavenport/dax/internal/prompt"
	"github.com/ChristopherDavenport/dax/policy"
)

// assembly is a session's extensions, built: their tools in order, as
// the executor's adapters, and what the policy and the sub-agents need
// of them.
type assembly struct {
	set             *executor.Set
	tools, readOnly []agenttool.Tool
	matchers        map[string]agentpolicy.ToolMatcher
	aliases         map[string][]string
	shipped         []policy.Shipped
	hooks           []func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error)
}

// mcpPrefixLower is the prefix of every MCP server's tools, which no
// extension may take, so the user's mcp__* rules reach only servers.
const mcpPrefixLower = "mcp__"

// build builds the extensions' tools over env, in this process, and
// checks what each claims (buildFrom). On an error the tools built are
// closed.
func build(exts []extension.Extension, env extension.ToolEnv) (*assembly, error) {
	x, err := executor.InProcess(exts, env)
	if err != nil {
		return nil, err
	}
	a, err := buildFrom(context.Background(), exts, env, x, false)
	if err != nil {
		x.Close()
		return nil, err
	}
	return a, nil
}

// buildFrom binds x's tools, the extensions' wherever they run, and
// checks what each extension claims: its name, its tools, the names it
// owns and its aliases are unique across the session without regard to
// case; a read-only tool, a matcher and an alias name only the
// extension's own tools; a call a tool's facts claim names that is not
// one of them asks (factspolicy.SubjectsOf); a tool names an extension
// of the session. The extensions' matchers and hooks are built over
// env. With remote, x runs the tools elsewhere and env is this
// machine's view of it, so a matcher that gives a tool's subjects,
// which would read env, is refused: what a served call touches is the
// executor's to say, through the tool's facts claim. It closes nothing:
// x is the caller's.
func buildFrom(ctx context.Context, exts []extension.Extension, env extension.ToolEnv, x executor.Executor, remote bool) (*assembly, error) {
	set, err := executor.Bind(ctx, x)
	if err != nil {
		return nil, err
	}
	a := &assembly{set: set, matchers: map[string]agentpolicy.ToolMatcher{}, aliases: map[string][]string{}}
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
			return nil, errors.New("an extension with no name")
		}
		if extNames[e.Name] {
			return nil, fmt.Errorf("two extensions named %s", e.Name)
		}
		extNames[e.Name] = true
		own := map[string]bool{}
		var tools []executor.Bound
		for _, t := range set.Tools() {
			if t.Extension != e.Name {
				continue
			}
			tools = append(tools, t)
			a.tools = append(a.tools, t.Adapter)
			if err := claim(e.Name, t.Name(), "tool"); err != nil {
				return nil, err
			}
			own[t.Name()] = true
		}
		listed := map[string]bool{}
		readOnly := func(t executor.Bound) error {
			// The annotations are hints, never enough to allow; they
			// may make a decision stricter, as refusing this one does.
			if an := t.Annotations; !an.ReadOnly && an.Destructive {
				return fmt.Errorf("extension %s: %q is read-only but annotated destructive", e.Name, t.Name())
			}
			listed[t.Name()] = true
			a.readOnly = append(a.readOnly, t.Adapter)
			return nil
		}
		for _, n := range e.ReadOnly {
			i := slices.IndexFunc(tools, func(t executor.Bound) bool { return t.Name() == n })
			if i < 0 {
				return nil, fmt.Errorf("extension %s: read-only %q is not one of its tools", e.Name, n)
			}
			if err := readOnly(tools[i]); err != nil {
				return nil, err
			}
		}
		// An executor may say a tool is read-only that the extension's
		// list does not name; it is checked as one the list names.
		for _, t := range tools {
			if t.ReadOnly && !listed[t.Name()] {
				if err := readOnly(t); err != nil {
					return nil, err
				}
			}
		}
		for _, n := range e.Owns {
			if err := claim(e.Name, n, "tool"); err != nil {
				return nil, err
			}
			own[n] = true
		}
		ruleNames := slices.Collect(maps.Keys(own))
		for alias, targets := range e.Aliases {
			if err := claim(e.Name, alias, "alias"); err != nil {
				return nil, err
			}
			for _, t := range targets {
				if !own[t] {
					return nil, fmt.Errorf("extension %s: alias %s names %q, which is not one of its tools", e.Name, alias, t)
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
				return nil, fmt.Errorf("extension %s: matcher for %q, which is not one of its tools", e.Name, name)
			}
			if remote && m.Subjects != nil && slices.ContainsFunc(tools, func(t executor.Bound) bool { return t.Name() == name }) {
				return nil, fmt.Errorf("extension %s: the matcher for %q gives its subjects, which would read this machine; with an executor a served tool's subjects come from its facts claim", e.Name, name)
			}
		}
		// What a call of a tool that claims is matched as comes from its
		// facts claim, whichever extension's it is: the policy reads the
		// machine only through the tools. A claim may name only this
		// extension's own names, so it cannot borrow another's rules.
		adapters := make([]agenttool.Tool, len(tools))
		for i, t := range tools {
			adapters[i] = t.Adapter
		}
		ms, err = factspolicy.Matchers(adapters, ruleNames, ms)
		if err != nil {
			return nil, fmt.Errorf("extension %s: %w", e.Name, err)
		}
		maps.Copy(a.matchers, ms)
		for _, n := range e.Lifts {
			if !own[n] {
				return nil, fmt.Errorf("extension %s: lifts %q, which is not one of its tools", e.Name, n)
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
	for _, t := range set.Tools() {
		if !extNames[t.Extension] {
			return nil, fmt.Errorf("tool %q names extension %q, which the session does not have", t.Name(), t.Extension)
		}
	}
	// The rewrite a call runs with if allowed comes from its tool's facts
	// claim, as its subjects do.
	if h := factspolicy.Hook(a.tools); h != nil {
		a.hooks = append([]func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error){h}, a.hooks...)
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
	return e.s.childPolicy(name, e.eng, e.a.hook(), e.a.set)
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
