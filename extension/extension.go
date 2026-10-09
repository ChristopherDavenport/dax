// Package extension is what dax is made of. Everything a dax session
// offers the model, past the session itself (the model, the store, the
// policy engine, the workspace, AGENTS.md, MCP servers and compaction),
// comes from an Extension: the coding tools are dax-coding's, the
// explore and task sub-agents dax-agents', skills dax-skills', memory
// dax-memory's. None of them is special. Each is built from what this
// package offers, and a program built on dax adds its own on the same
// terms, or leaves one of dax's out.
//
// A session builds its extensions in two phases. First every
// extension's tools, over a ToolEnv. Then each extension's kit options,
// over an Env that sees every extension's tools, so a sub-agent built
// in the second phase can be given a tool an extension listed after
// it adds.
//
// The package is pre-1.0: its API may change in a minor version, and
// dax's CHANGELOG says when.
package extension

import (
	"context"
	"fmt"

	"github.com/ChristopherDavenport/agentconsole/toolview"
	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/policy"
	"github.com/ChristopherDavenport/dax/tool"
	"github.com/ChristopherDavenport/dax/workspace"
)

// Extension is one part of a dax session. Every field is optional but
// Name.
//
// An extension's tools run under the session's policy like any other:
// a call no rule allows asks first. The extension's own rules (Policy)
// are recorded under a source of their own, so a verdict one decides
// names the extension, and they may name only the tools the extension
// owns: its Tools, the names in Owns, and its
// Aliases. A tool name, an alias, and a name in Owns are unique across
// the session's extensions, compared without regard to case.
type Extension struct {
	// Name names the extension, dax-coding say; its rules are recorded
	// under policy.SourceExtension(Name). The extensions dax ships are
	// named dax-<what>.
	Name string

	// Tools are offered to the main agent, and to a sub-agent built
	// from Env.Tools, task's say, in this order.
	Tools func(ToolEnv) []agenttool.Tool
	// ReadOnly names the Tools that only look, which a sub-agent built
	// from Env.ReadOnlyTools gets too, explore's say: a tool none of
	// whose calls changes anything, or one whose calls that would the
	// policy asks about, as dax-coding's bash. A tool whose annotations
	// call it destructive may not be named here.
	ReadOnly []string
	// Owns names the tools the extension adds through its kit options
	// (skill, memory_save), so its rules may name them and no other
	// extension may take the names.
	Owns []string

	// Matchers say how a rule's specifier matches a call of one of the
	// extension's tools: bash(git status:*) is dax-coding's bash matcher
	// reading the command. A tool without one is matched by name alone.
	// What a call is matched as, its subjects, is the tool's own facts
	// claim (agenttool.Factual) when it makes one: the session reads the
	// claim, so a matcher for a claiming tool supplies Match alone, and
	// one that brings Subjects too is an error at start. A tool with no
	// claim is matched on its arguments as given, or on a matcher's own
	// Subjects. They are built over the session's ToolEnv, as the tools
	// are.
	Matchers func(ToolEnv) map[string]agentpolicy.ToolMatcher
	// Aliases are names a rule may use for several of the extension's
	// tools: dax-coding's Read stands for read, grep, glob and ls.
	Aliases map[string][]string
	// Policy are the rules the extension ships. They are trusted and
	// rank with every extension's, below the user's config: an
	// extension's rule cannot carve out another source's, a user's
	// carve-out can lift one of the extension's, and the config's
	// "builtin": false drops the allow rules (the asks and denies stay).
	// A rule may name only the extension's own tools and aliases, and
	// may not be a carve-out.
	Policy policy.Rules
	// Lifts are the extension's tools whose asks a user's allow rule
	// with a specifier lifts: allowing read(.env) means reading .env,
	// though dax-coding asks about secret-looking paths.
	Lifts []string
	// BeforeToolCall decides or rewrites a call, as
	// agentkit.WithBeforeToolCall does, for the main agent and for every
	// sub-agent built with Env.ChildPolicy. Decisions fold deny over ask
	// over allow with the policy's, so it can make a call stricter but
	// not allow one the policy asks about. The rewrite a tool's facts
	// claim asks for (bash's stamped plan) is applied by the session
	// this way, with no hook of the extension's. It is built over the
	// session's ToolEnv, as the tools are.
	BeforeToolCall func(ToolEnv) func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error)

	// Instructions are added to the main agent's system prompt, after
	// dax's role line and in the order the extensions are given, before
	// the user's own instructions file. A sub-agent's prompt is its
	// extension's to build (Env.SystemPrompt).
	Instructions string

	// Kit returns agentkit options for what the fields above do not
	// cover: skills, memory, child agents, guards, hooks. It runs after
	// every extension's tools are built. The session owns the kit's
	// name, model, reasoning, instructions, policy, session and
	// compaction, and its own options go to the kit after the
	// extensions', so where an option replaces, the session's is the
	// one in force. An option that sets a policy on a session whose
	// policy is off is an error at start.
	Kit func(Env) ([]agentkit.Option, error)

	// Renderers draw the extension's tool calls in the terminal client,
	// by tool name, for a session rooted at dir. A renderer reads only
	// the record, and declines (ok false) a call whose arguments or
	// schema it was not written for, such as one an older version
	// recorded; the client then draws the call as raw text. Two
	// extensions may not draw one tool.
	Renderers func(dir string) toolview.Renderers
}

// ToolEnv is what an extension's tools are built over.
type ToolEnv struct {
	// Workspace is where the tools act: this machine, a container or a
	// remote runtime, behind one interface. A tool reads and writes its
	// files and runs its processes through it, never through os or
	// os/exec, so it runs unchanged wherever the session's workspace is.
	// The session closes a workspace it opened.
	Workspace workspace.Workspace
	// Files is the session's view of Workspace for paths as the model
	// writes them (absolute in the workspace's root, or relative to
	// it), with the write lock dax's own write and edit hold. A tool
	// that takes a path from the model goes through it, so a path out
	// of the workspace is refused.
	Files *tool.Files
	// MaxReadBytes is the most of a file a tool should read; zero is
	// tool.DefaultMaxRead.
	MaxReadBytes int64
}

// Env is the session as an extension's kit options see it.
type Env interface {
	// ToolEnv is what the tools were built over.
	ToolEnv() ToolEnv
	// Name is the program's, dax unless it named itself.
	Name() string
	// Dir is the directory on this machine the session started in
	// (agent.Options.Dir); the tools act in ToolEnv().Workspace, which
	// may be elsewhere, and the project's files are read through it,
	// not from Dir. UserDir is the user's ~/.dax.
	Dir() string
	UserDir() string
	// Tools are every extension's tools, the read-only ones included,
	// in extension order; ReadOnlyTools are those alone.
	Tools() []agenttool.Tool
	ReadOnlyTools() []agenttool.Tool
	// SystemPrompt is the main agent's part of the system prompt, with
	// the instructions of the extensions named in except left out: a
	// sub-agent's prompt is built from it.
	SystemPrompt(except ...string) string
	// AgentsMD is the AGENTS.md chain as the main agent's prompt carries
	// it, empty when AGENTS.md is off.
	AgentsMD() string
	// Model is the session's model, and Live the model name and the
	// reasoning setting in force now, which /model and /think change
	// while the session runs.
	Model() openresponses.Streamer
	Live() (model string, think bool)
	// Reasoning is the reasoning a request to model asks for under
	// think, fitted to what the model's vendor says it takes.
	Reasoning(ctx context.Context, model string, think bool) openresponses.ReasoningConfig
	// ChildPolicy is the BeforeToolCall hook of a sub-agent called name:
	// the session's policy and every extension's BeforeToolCall, with a
	// call the policy asks about put to the user from inside the run.
	// Nil when the session has no policy.
	ChildPolicy(name string) func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error)
	// Omit reports an instruction source the extension left out, as the
	// start lines show them: a repository's skills directory that is a
	// link out of it, say.
	Omit(o agentkit.Omission)
	// Log is a note for the user, as the front shows them.
	Log(format string, args ...any)
}

// Renderers are the extensions' renderers for a session rooted at dir,
// for a front to hand the terminal client. Two extensions that draw one
// tool are an error.
func Renderers(dir string, exts []Extension) (toolview.Renderers, error) {
	out := toolview.Renderers{}
	by := map[string]string{}
	for _, e := range exts {
		if e.Renderers == nil {
			continue
		}
		for name, r := range e.Renderers(dir) {
			if by[name] != "" {
				return nil, fmt.Errorf("extensions %s and %s both draw %s", by[name], e.Name, name)
			}
			by[name] = e.Name
			out[name] = r
		}
	}
	return out, nil
}

// FixedMatchers is a Matchers for matchers that read only a call's
// arguments, a glob over one of them say, and so need nothing of the
// session.
func FixedMatchers(m map[string]agentpolicy.ToolMatcher) func(ToolEnv) map[string]agentpolicy.ToolMatcher {
	return func(ToolEnv) map[string]agentpolicy.ToolMatcher { return m }
}

// FixedHook is a BeforeToolCall for a hook that needs nothing of the
// session.
func FixedHook(h func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error)) func(ToolEnv) func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
	return func(ToolEnv) func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) { return h }
}
