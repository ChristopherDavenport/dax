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
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/ChristopherDavenport/agentconsole/toolview"
	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/policy"
	"github.com/ChristopherDavenport/dax/tool"
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
	//
	// They are the session's execution: what touches the world. The
	// session runs them through its executor, which today is these
	// tools in this process and may be another machine, so what the
	// kit, the policy and Env.Tools hold are stand-ins whose calls,
	// facts claims and replay claims go to the executor; a tool's own
	// value is the executor's, which closes it. Tools an extension
	// adds through Kit (memory, the sub-agents, skill) are control: they
	// run where the session does, and never go to the executor.
	Tools func(ToolEnv) []agenttool.Tool
	// ReadOnly names the Tools that only look, which a sub-agent built
	// from Env.ReadOnlyTools gets too, explore's say: a tool none of
	// whose calls changes anything, or one whose calls that would the
	// policy asks about, as dax-coding's bash. A tool whose annotations
	// call it destructive may not be named here.
	ReadOnly []string
	// Owns names the tools the extension adds through its kit options
	// (skill, memory_save), so its rules may name them and no other
	// extension may take the names. A call of one whose tool makes the
	// facts claim (agenttool.Factual) is decided on that claim, as a
	// call of one of Tools is, once the session's kit is built.
	Owns []string
	// SubagentTools are tools of the extension's that the sub-agents
	// other extensions build may take too (Env.SubagentTools): control
	// tools that only read, such as dax-memory's memory_search over the
	// session's memory scopes, which explore and task are given beside
	// Env.Tools. Each is one of the names in Owns, decided under that
	// name's rules, and annotated read-only and not destructive. They are
	// built over the session's Env before any extension's Kit runs.
	SubagentTools func(Env) ([]agenttool.Tool, error)
	// PersonOnly names tools of the extension's (its Tools or names in
	// Owns) that read or change what is the person's own beyond the
	// workspace and this conversation: their memory, the skills the
	// model wrote for them. An A2A caller's conversation, and its
	// sub-agents', is refused every call of one outright, whatever the
	// rules say, with policy.NoWorkAround's note, and no one is asked
	// (agent's personOnly): one approval would otherwise let a caller
	// read, or write into, what every later session of the person's
	// reads. dax-memory names its memory tools and dax-skills
	// skill_manage.
	PersonOnly []string

	// Matchers say how a rule's specifier matches a call of one of the
	// extension's tools: bash(git status:*) is dax-coding's bash matcher
	// reading the command. A tool without one is matched by name alone.
	// What a call is matched as, its subjects, is the tool's own facts
	// claim (agenttool.Factual) when it makes one: the session reads the
	// claim, so a matcher for a claiming tool supplies Match alone, and
	// one that brings Subjects too is an error at start. A tool with no
	// claim is matched on its arguments as given, or on a matcher's own
	// Subjects, which, as a claim, may name only the extension's own
	// tools: a subject that names another's asks. They are built over
	// the session's ToolEnv, as the tools are.
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
	// ModeRules are the rules the extension adds in a permission mode,
	// by the mode's name; only policy.ModeAcceptEdits takes any:
	// dax-coding allows write and edit there and asks about the paths
	// that change what runs or is trusted. They are held to the
	// extension's own tools and aliases as Policy is, and kept with
	// "builtin": false, since the user chose the mode.
	ModeRules map[string]policy.Rules
	// GrantRule is the rule a person's "yes, always" to a call of one
	// of the extension's tools (its Tools and Owns) grants, in the
	// policy's grammar, or "" to offer only "once": dax-coding offers
	// bash(<first two words>:*) for a line its analyzer reads whose
	// asking stages share them, and nothing for write or edit. A tool
	// with no extension's GrantRule is offered its bare name. The
	// session offers the rule only when no rule asked about the call (a
	// grant ranks below every source, policy.RankGrant). It is built
	// over the session's ToolEnv, as the tools are.
	GrantRule func(ToolEnv) func(ctx context.Context, tool string, args json.RawMessage) string
	// Lifts are the extension's tools whose asks a user's allow rule
	// with a specifier lifts: allowing read(.env) means reading .env,
	// though dax-coding asks about secret-looking paths.
	Lifts []string
	// HeldTo holds the calls of one of the extension's tools (a key:
	// one of its Tools or a name in Owns) to the ask and deny rules of
	// other extensions' tools (the value). A call a tool's facts claim
	// (agenttool.Factual) names of such a tool is a constraint on the
	// call (agentpolicy's Subject.Constrain): that tool's ask and deny
	// rules, whoever's they are, can make the call ask or refuse it,
	// and its allow rules never allow it, so the call still needs an
	// allow of its own tool's. A claim that names any other tool asks.
	// dax-skills holds skill to read: a project's skill file is
	// claimed as a read of it, so read(.env)'s question and a user's
	// deny of read(config/**) reach it, and an allow of read(...) does
	// not open it.
	HeldTo map[string][]string
	// BeforeToolCall decides or rewrites a call, as
	// agentkit.WithBeforeToolCall does, for the main agent and for every
	// sub-agent built with Env.ChildPolicy. Decisions fold deny over ask
	// over allow with the policy's, so it can make a call stricter but
	// not allow one the policy asks about. The rewrite a tool's facts
	// claim asks for (bash's stamped plan) is applied by the session
	// this way, with no hook of the extension's. It is built over the
	// session's ToolEnv, as the tools are.
	BeforeToolCall func(ToolEnv) func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error)

	// PolicyHooks are hooks the session's policy engine runs for every
	// call it decides, the main agent's and its sub-agents' alike
	// (agentpolicy's WithHooks and WithAnsweringHooks), built over the
	// session as the kit options are. With the policy off, Decide alone
	// runs, as a BeforeToolCall hook of the main agent.
	PolicyHooks func(Env) (PolicyHooks, error)

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

	// Secrets name the environment variables the extension reads
	// credentials from: dax-a2a's peers' token variables, say. The
	// session takes them out of the environment its tools and MCP
	// servers get, whatever their names, unless the user's pass_env
	// names one, and always out of the environment of the command that
	// starts an executor, as it does the model's key. A name that looks
	// like a credential's (*_TOKEN, *_API_KEY) is taken out anyway; one
	// that does not is only taken out if it is here.
	Secrets []string

	// Renderers draw the extension's tool calls in the terminal client,
	// by tool name, for a session rooted at dir. A renderer reads only
	// the record, and declines (ok false) a call whose arguments or
	// schema it was not written for, such as one an older version
	// recorded; the client then draws the call as raw text. Two
	// extensions may not draw one tool.
	Renderers func(dir string) toolview.Renderers
}

// PolicyHooks are an extension's hooks in the policy engine's decision
// of a call. Either may be nil. Each must decide a call the same way
// however often it is asked, since the engine asks again for a
// sibling of the call being decided.
type PolicyHooks struct {
	// Decide is folded into the policy's decision strictest-wins, deny
	// over ask over allow, and recorded with its decider (By): it can
	// refuse a call or ask about it, never allow one the policy asks
	// about or refuses.
	Decide func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error)
	// Answer may answer the policy's own ask with an allow, as a
	// person's yes would, and nothing else it returns counts. It lifts
	// only an ask of a source of rank 0 or below, the project's rules
	// and the fallback; never an extension's or the user's own ask
	// rules, a deny, an ask a Decide hook raised, nor any call of an
	// A2A caller's conversation, which is asked as before.
	Answer func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error)
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
	// HostEnv is the environment a process an extension starts on this
	// machine gets: this process's, less every secret the session keeps
	// from its children (SecretsOf, the model's key's variable, the MCP
	// servers' token and secret variables) unless pass_env keeps it.
	HostEnv() []string
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
	// SubagentTools are every extension's SubagentTools, in extension
	// order, cut to their caps as Tools are: what a sub-agent may read
	// of the session beyond the workspace, such as its memory.
	SubagentTools() []agenttool.Tool
	// SearchScope confines a search of the session store, agentkit's
	// conversation_search say (agentkit.SearchScope), to what the
	// call's caller may read: for the person, this project's top-level
	// sessions; for an A2A caller's conversation, none, the call
	// failing. It narrows q and never widens it. An extension that
	// searches the store sets it on every search, so a caller never
	// reads the person's conversations.
	SearchScope(ctx context.Context, q *agentsession.Query) error
	// Caller says whether ctx is an A2A caller's conversation, or one of
	// its sub-agents' (dax serve -a2a), and names it: who and the
	// session that records it. A caller is not the person: an extension
	// that holds the person's own data beyond the workspace shows a
	// caller none of it, as dax-memory's store does, which reads empty
	// on a caller's context, so a caller's requests carry no memory
	// block (PersonOnly refuses the tools).
	Caller(ctx context.Context) (who string, ok bool)
	// ReloadSkills asks the session to discover its skills again
	// (agentkit's Kit.ReloadSkills) at the main agent's next turn,
	// before its model call, when none of its calls is being decided:
	// a skill an extension wrote since is listed and served from that
	// request on, in the run that wrote it or the next. A failed reload
	// is a warning, and the catalogue in force stays.
	ReloadSkills()
	// SystemPrompt is the main agent's part of the system prompt, with
	// the instructions of the extensions named in except left out: a
	// sub-agent's prompt is built from it.
	SystemPrompt(except ...string) string
	// AgentsMD is the AGENTS.md chain as a request on ctx carries it:
	// the main agent's prompt's, and on an A2A caller's context (Caller)
	// the chain without the person's own files (~/.dax/AGENTS.md,
	// agents_md_global), as the caller's requests carry it. Empty when
	// AGENTS.md is off.
	AgentsMD(ctx context.Context) string
	// Model is the session's model, and Live the model name and the
	// reasoning setting in force now, which /model and /think change
	// while the session runs.
	Model() openresponses.Streamer
	Live() (model string, think bool)
	// SubagentModel is the model a sub-agent's requests go through:
	// Model, unless the session was given one of its own for them
	// (agent.Options.SubagentStreamer), such as one whose prompt cache
	// lives for another time. A sub-agent's configuration takes it in
	// place of Model.
	SubagentModel() openresponses.Streamer
	// Reasoning is the reasoning a request to model asks for under
	// think, fitted to what the model's vendor says it takes.
	Reasoning(ctx context.Context, model string, think bool) openresponses.ReasoningConfig
	// ChildBeforeTurn is the BeforeTurn hook a sub-agent's
	// configuration takes: the AGENTS.md files below the start
	// directory that the sub-agent's own work reaches, given as the main
	// agent's are, each once, recorded as composed from its file. Nil
	// when the session reads no AGENTS.md.
	ChildBeforeTurn() func(context.Context, agentturn.TurnStartInfo) (openresponses.Items, error)
	// ChildBeforeModelCall is the BeforeModelCall hook a sub-agent's
	// configuration takes: on an A2A caller's context (Caller) its
	// requests leave out what is the person's own, as the caller's own
	// requests do (their AGENTS.md, the definitions of the PersonOnly
	// tools); and a read's or the person's images are said, not shown,
	// to a sub-agent model that does not take images, so the
	// sub-agent's record holds what its model was sent.
	ChildBeforeModelCall() func(context.Context, *openresponses.Request) error
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
	// ToolOutput is the whole of a tool result the session cut to its
	// cap, by the output id the cut result names (out_<hex>): only an
	// output this session kept.
	ToolOutput(ctx context.Context, ref string) ([]byte, error)
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

// SecretsOf are the Secrets of exts, in order, each once.
func SecretsOf(exts []Extension) []string {
	var out []string
	for _, e := range exts {
		for _, n := range e.Secrets {
			if n != "" && !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
	}
	return out
}

// textSchema is the form of a TextQuestion: one string property,
// "answer", and dax's mark, which tells it from anyone else's form.
const textSchema = `{"type":"object","x-dax":"text-question","properties":{"answer":{"type":"string"}},"required":["answer"]}`

// TextQuestion is a question a tool asks the user for a line of text,
// not a yes or a no: a remote agent's "which colour?". It is a form of
// one string property with dax's mark, which every front of dax's
// answers with what the user typed: a reply that is not an explicit no
// is the answer, and a bare yes is "yes". Read the answer with
// TextAnswer. Only this form is answered so; any other form, an MCP
// server's of one text field included, is cancelled as before, since
// its constraints (an enum, a pattern) are not the fronts' to check.
func TextQuestion(message string) agenttool.Elicitation {
	return agenttool.Elicitation{Message: message, Schema: json.RawMessage(textSchema)}
}

// IsTextQuestion reports whether q is a TextQuestion: its form is
// exactly the one TextQuestion builds, mark included, however the JSON
// was spaced or ordered on its way.
func IsTextQuestion(q agenttool.Elicitation) bool {
	if len(q.Schema) == 0 || q.URL != "" {
		return false
	}
	var got, want any
	if json.Unmarshal(q.Schema, &got) != nil || json.Unmarshal([]byte(textSchema), &want) != nil {
		return false
	}
	return reflect.DeepEqual(got, want)
}

// TextAnswer is the text an accepted answer to the TextQuestion q gave:
// its form's field, or, from a front that answers with a note, the
// note, or "yes" for a bare yes. ok is false for a decline, a cancel,
// or a question that is not a TextQuestion.
func TextAnswer(q agenttool.Elicitation, a agenttool.Answer) (text string, ok bool) {
	if a.Action != agenttool.ActionAccept || !IsTextQuestion(q) {
		return "", false
	}
	if len(a.Content) > 0 {
		var form map[string]any
		if json.Unmarshal(a.Content, &form) == nil {
			if s, ok := form[textField].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s), true
			}
		}
	}
	if n := strings.TrimSpace(a.Note); n != "" {
		return n, true
	}
	return "yes", true
}

// TextContent is the form a front answers a TextQuestion with: text as
// its field.
func TextContent(text string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{textField: text})
	return b
}

// textField is a TextQuestion's one property.
const textField = "answer"

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
