// Package agent assembles one dax session with agentkit: the model, the
// prompt frame, the AGENTS.md chain, the confirmation policy, MCP
// servers and compaction, recorded into an agentsession
// content-addressed store (RFC 0002), and the extensions that offer
// everything else (Options.Extensions; see package extension). The
// session knows no tool: dax's file tools and bash are dax-coding's, the
// sub-agents dax-agents', and so on. What the kit cannot express is
// wired here by hand beside it, each place saying why.
//
// The package is pre-1.0 and its API may change between minor versions.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	"github.com/ChristopherDavenport/agentsmd"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agenttool/mcpclient"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/compact"
	"github.com/ChristopherDavenport/agentturn/session"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/agentworkspace/mcpworkspace"
	"github.com/ChristopherDavenport/agentworkspace/remote"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/internal/cmdline"
	"github.com/ChristopherDavenport/dax/internal/config"
	"github.com/ChristopherDavenport/dax/internal/executor"
	"github.com/ChristopherDavenport/dax/internal/offer"
	"github.com/ChristopherDavenport/dax/internal/render"
	"github.com/ChristopherDavenport/dax/policy"
	"github.com/ChristopherDavenport/dax/tool"
)

// Version is what the session header names as the harness version.
const Version = "0.1.0-dev"

// What the model sees for a call that will never get a real answer.
// Each says what the loop's PendingReason says and no more: a call cut
// off in flight may have taken effect, a deferred call never ran, and
// a call found unanswered in a transcript the record says nothing
// about is one nobody can account for.
const (
	abortedOutput    = "Error: this call was cut off while it was running; it may have taken effect."
	deniedOutput     = "Error: the user denied this call. " + policy.NoWorkAround
	unknownOutput    = "Error: an earlier run left this call unanswered; whether it ran is not recorded."
	neverRanOutput   = "Error: this call was cut off before it reached its tool; it did not run."
	unansweredOutput = "Error: this call needed approval and nobody was there to give it, so it did not run. " + policy.NoWorkAround
	heldOutput       = "Error: this call was waiting for approval when the session stopped, and may have run before it was held; it will not run again."
)

// Options configure a session.
type Options struct {
	// Name and Version are what the session calls its harness: in the
	// system prompt, to the kit, and in the session header. Empty is
	// dax and Version. A program built on dax gives its own.
	Name, Version string
	// Description is what the kit says the agent is; empty is dax's
	// for dax, and a plain line for a program that named itself.
	Description string
	// Extensions are everything the session offers the model past
	// itself, in order: dax's command line gives dax-coding, and
	// dax-agents, dax-skills and dax-memory as the settings say. With
	// none the model has no tools but MCP servers'.
	Extensions []extension.Extension
	// Streamer is the model; dax's command line builds one from the
	// configuration.
	Streamer openresponses.Streamer
	// SubagentStreamer is the model the sub-agents' requests go through
	// (extension.Env's SubagentModel); nil is Streamer. dax's command
	// line gives one of its own when the sub-agents' prompt cache lives
	// for another time than the main conversation's (subagent_cache_ttl).
	// Compaction and an A2A caller's conversation go through Streamer.
	SubagentStreamer openresponses.Streamer
	// Model is the model name the requests carry.
	Model string
	// MaxTurns stops each run after this many turns, stopped with
	// cause max_turns; zero is no limit. The sub-agents keep their own.
	MaxTurns int
	// MaxTokens and MaxCost stop a run, stopped with cause guard,
	// before its next model call once the session's model calls, the
	// sub-agents' among them, have used this many tokens (input and
	// output) or cost this many US dollars by Price; a sub-agent's next
	// call is refused then too. Zero is no limit. Under MaxCost a model
	// Price does not know refuses the session's start, or stops the run
	// once a call goes to one.
	MaxTokens int
	MaxCost   float64
	Price     Price
	// PricedModels are the other models the session's calls go to (the
	// sub-agents'), each of which Price must know under MaxCost, checked
	// at start as Model is.
	PricedModels []string
	// Fit is the reasoning effort to ask a model for in place of the
	// one Think implies, from what its vendor says it takes; nil asks
	// as configured. It is applied where each configuration is built,
	// never to a request on its way out, so the session records the
	// request that was sent.
	Fit func(ctx context.Context, model string, want openresponses.ReasoningEffort) openresponses.ReasoningEffort
	// TakesImages reports whether model takes images as input, as its
	// vendor says (internal/modelinfo). An image a file read returns,
	// or one the person attaches, is sent only to a model it says yes
	// for; nil is never.
	TakesImages func(ctx context.Context, model string) bool
	Think       bool
	// Effort is the reasoning effort Think asks for; empty is low.
	Effort openresponses.ReasoningEffort
	// Dir is the directory on this machine the session started in.
	// With no Workspace it is where the tools act, as a
	// workspace.Local. The project's instructions (AGENTS.md, an
	// extension's project files such as .dax/skills) are read through
	// the workspace, not from Dir; only when the workspace is Dir on
	// this machine and Dir is below its repository's root are the
	// AGENTS.md files between the two read from here.
	Dir string
	// Workspace is where the tools act and what the session records as
	// its cwd and workspace: a container, a remote runtime, or this
	// machine. Nil is a workspace.Local over Dir, with an environment
	// scrubbed of credentials (PassEnv, KeyEnv), which the session
	// closes; one given here is the caller's to close.
	Workspace workspace.Workspace
	// ServersWorkspace is a second workspace beside Workspace (or the
	// executor's), whose offered servers (workspace.ServerProvider) the
	// session connects and nothing else: a served or tunnelled one
	// granted servers alone (config.ServersAlone), so a server holding a
	// secret of another machine's is used while the tools act in the
	// session's own workspace. Its servers are connected as the
	// workspace's offered ones are, through mcpStart, under the same
	// names, collisions, rules and restarts; no tool acts in it, no file
	// of it is read, and its servers are recorded under its identity in
	// the session's header. One that offers none, that is the
	// workspace, or that grants more than servers (exec, which reads a
	// running server's environment, or write, which changes its code)
	// fails the session. It is the caller's to close, after the session.
	ServersWorkspace workspace.Workspace
	// ServersWorkspaces are more servers workspaces, by name (config's
	// servers_workspaces), each held to ServersWorkspace's rules, which
	// is named ServersName beside them: and across them, no two are one
	// workspace and no server name is offered by two. A name is a DNS
	// label. Each is the caller's to close, after the session.
	ServersWorkspaces map[string]workspace.Workspace
	// Executor runs the extensions' tools elsewhere, in `dax execute`
	// (DialExecutor), in place of building them in this process over a
	// workspace: their calls, facts and stamps are the executor's, and
	// the session records its workspace. Setting both Executor and
	// Workspace is an error. Every extension with Tools must be one the
	// executor runs, and no extension's matcher may give a served
	// tool's subjects (the policy reads the executor only through the
	// tools' facts). MCP servers start in the executor (its workspace
	// is a workspace.Starter there), and an executor that cannot start
	// one fails them, never starting them here. The project's files
	// are read through it. It is the caller's to close, after the
	// session.
	Executor *Executor
	// BoxConfine is the session's box.confine, with a box host in front
	// of Executor: config.ConfineUnits or config.ConfineWorkspace has the
	// policy take that box's tools as confined (confine.go), so a bare
	// ask rule for them does not ask. Empty or config.ConfineOff, and
	// any value without a box host's envelope, claims nothing.
	BoxConfine string
	// Store records the session: a store the caller opened, which it
	// closes after the session; a remote one (agentsession RFC 0003)
	// fits here as a local one does. Nil opens the content-addressed
	// store at Root.
	Store agentsession.Store
	// Root is the local session store, a content-addressed store
	// holding every project's sessions, used when Store is nil; both
	// empty disables recording.
	Root string
	// Sync is when an append to the store at Root is durable before it
	// returns: every append (the default), a response or a call's
	// output and what came before it, or at Close.
	Sync cas.SyncPolicy
	// UserDir is the user's dax directory, ~/.dax: its AGENTS.md is
	// read before the project's.
	UserDir string
	// Instructions is text of the user's own, from the config's
	// instructions_file, added to dax's part of the system prompt.
	Instructions string

	// AgentsMD reads UserDir/AGENTS.md, AgentsMDGlobal and the
	// AGENTS.md chain from the repository's root down to the
	// workspace's root into the instructions (https://agents.md);
	// never a file above the repository.
	AgentsMD bool
	// AgentsMDGlobal are the user's own instruction files for every
	// session (the config's agents_md_global), read under AgentsMD
	// after UserDir/AGENTS.md and before the chain, in order. They are
	// paths on this machine whatever the workspace, since they are the
	// user's and not the project's, and are not screened; a missing
	// one is skipped.
	AgentsMDGlobal []string
	// NoUserAgentsMD leaves UserDir/AGENTS.md out of the chain under
	// AgentsMD (dax -bare); the repository's files are still read.
	NoUserAgentsMD bool
	// BashMaxOutput and ToolMaxOutput cap what a bash call's result, and
	// a result of a tool that is not dax's own, send the model, in bytes
	// (bash_max_output, mcp_max_output); 0 is config's default. What is
	// cut is kept with the session (Session.ToolOutput).
	BashMaxOutput, ToolMaxOutput int
	// OneShot is a run nobody replies to (-p): the model is told so in a
	// part of its own (PartOneShot), recorded with the instructions, so
	// it finishes the task rather than ending on a question.
	OneShot bool
	// Bare names the sources a bare run (dax -bare) left out, which the
	// header records as dax.profile so two sessions read as the
	// configurations they were; nil for a run that is not bare.
	Bare []string
	// Compact, when positive, is the estimated token budget above which
	// the transcript is folded before a call.
	Compact int
	// CompactFrom says where Compact came from, for the header's
	// dax.context: "flag" (-compact) or "window" (three quarters of
	// Window); empty with Compact 0 is "off".
	CompactFrom string
	// Window is the context window, in tokens, dax believes the model
	// has, 0 unknown, and WindowFrom where that came from: the vendor's
	// figure ("vendor:<name>"), Ollama's served window, or the user's
	// (config, flag). Both are recorded in the header's dax.context.
	Window     int64
	WindowFrom string
	// CompactServer folds through the server's compaction endpoint
	// (compact.New) instead of a local summary (compact.NewLocal).
	CompactServer bool
	// Policy is the user's and the project's rules; the session adds
	// the extensions' (Settings.Shipped) and builds the policy that
	// decides which calls run, ask or are refused. nil lets every call
	// run, and the extensions' BeforeToolCall hooks are not run.
	Policy *policy.Settings
	// MCP are the MCP servers connected with the session, each offering
	// its tools as mcp__<Name>__<tool>. A command's server runs in the
	// workspace when it can start a process (workspace.Starter), at its
	// root and with its environment, and otherwise on this machine; with
	// an Executor it runs in the executor, and one that cannot start a
	// process fails the session. A server placed on the host runs on
	// this machine whatever the workspace, and one at a URL is called
	// from this machine. The servers the workspace offers of its own
	// (workspace.ServerProvider: a box host's units) are connected
	// beside these. A project's servers start only when TrustMCP is set
	// or the trust file holds their set for this workspace. Their stderr
	// is dax's, cleaned.
	MCP []MCPServer
	// TrustMCP trusts the set of MCP servers the project's config names
	// for this workspace, starts them, and records the trust in
	// MCPTrustFile, so a later session of the project with the same set
	// starts them without it (-trust-mcp).
	TrustMCP bool
	// MCPTrustFile is where the trusted sets are kept; empty is
	// DefaultMCPTrustFile under DefaultUserDir.
	MCPTrustFile string
	// NoMCPTrustFile neither reads nor writes the trust file (dax -bare):
	// a project's servers start only with TrustMCP, for this session.
	NoMCPTrustFile bool
	// MaxReadBytes is the most bytes of a file a tool should read,
	// which extension.ToolEnv carries; zero is tool.DefaultMaxRead.
	// With an Executor, its tools take ExecutorOptions.MaxReadBytes,
	// given when it was dialed, and not this.
	MaxReadBytes int64
	// PassEnv names credential-looking variables the processes of the
	// workspace.Local the session opens when Workspace is nil, MCP
	// servers among them, may still inherit; every other credential is
	// removed from their environment. A workspace given in Workspace
	// carries its own (Workspace.Env), and an MCP server that cannot
	// start in it runs on this machine with an environment scrubbed the
	// same way.
	PassEnv []string
	// KeyEnv is the variable the provider's key was read from. It is
	// removed from the same environments even when its name does not
	// look like a credential's, unless PassEnv names it, as are the
	// variables the extensions name in Secrets.
	KeyEnv string
	// Log receives dax's own notes: compactions, denials, skill grants.
	// nil discards them.
	Log func(format string, args ...any)
	// ViewGrace is how long an A2A caller's conversation's question
	// waits for a view to attach again once the last has gone, as one
	// reconnecting does, before it is given up; zero is
	// DefaultViewGrace.
	ViewGrace time.Duration

	// executor runs the extensions' tools in place of the session's
	// in-process one, which builds them over the workspace; the session
	// closes it. It is the seam the tests use to stand in for an
	// executor elsewhere.
	executor executor.Executor
}

// MCPServer is an MCP server to connect: a command's stdio server, or
// one at a URL.
type MCPServer struct {
	// Name is the prefix of the server's tools.
	Name string
	// Command is the command line, split as a shell splits words with
	// nothing expanded; no shell runs it.
	Command string
	// Host runs Command on this machine whatever the workspace, with
	// this machine's environment scrubbed, even with an Executor: a
	// server called independently of the sandbox.
	Host bool
	// URL is a streamable HTTP server called from this machine, in
	// place of a command; TokenEnv names the variable whose token is
	// sent to its origin alone, and which every child's environment
	// loses.
	URL, TokenEnv string
	// Project says the project's config named the server: it starts only
	// when the user trusts the project's set (Options.TrustMCP).
	Project bool
	// Description says what the server is for, for a person reading a
	// list of servers.
	Description string
	// offeredBy is the workspace that offers the server of its own
	// (workspace.ServerProvider), the session's or the servers
	// workspace: it is started by name there, and the session sets it,
	// never a caller. offeredAs and offeredAt are that placement as
	// Info.MCP lists it.
	offeredBy workspace.ServerProvider
	offeredAs Placement
	offeredAt string
	// offeredSecrets is the offering's mark that it gives the server
	// secrets there, for the listing.
	offeredSecrets bool
	// SecretEnv names variables a command's server is given from this
	// process's environment, which every other child of the session
	// loses (Secrets): it is started as a workspace's offered server is
	// (agentworkspace's Offer), its output, standard output and standard
	// error, redacted of their values before the session reads any of
	// it. A secret stays on the machine whose environment holds it, so
	// such a server must be placed on this machine (Host), never in a
	// workspace; not for a project's server or a URL; and none may be
	// KeyEnv, a TokenEnv, an extension's secret, a pass list's or a
	// served workspace's token variable.
	SecretEnv []string
}

// Placement is where one of a session's MCP servers runs, as Info.MCP
// lists it.
type Placement string

// The placements an MCP server has. Whichever it is, the server is
// connected the one way (its tools mcp__<name>__<tool>, its restarts,
// its output cleaned), so a listing differs only in this.
const (
	// PlacedHost is a command on this machine: placed on the host, one
	// holding secrets, or one beside a workspace that cannot start a
	// process.
	PlacedHost Placement = "host"
	// PlacedWorkspace is a command started in the session's workspace:
	// this directory, an executor, a box, or a served or tunnelled
	// workspace whose grant runs commands.
	PlacedWorkspace Placement = "workspace"
	// PlacedURL is a streamable HTTP server, called from this machine.
	PlacedURL Placement = "url"
	// PlacedOffered is a server the session's workspace offers of its
	// own: a box host's unit, or a served or tunnelled workspace's
	// -servers.
	PlacedOffered Placement = "offered"
	// PlacedServersWorkspace is a server the servers workspace offers
	// (Options.ServersWorkspace).
	PlacedServersWorkspace Placement = "servers workspace"
)

// MCPServerInfo is one of a session's MCP servers as Info.MCP lists
// it: the same fields wherever it runs.
type MCPServerInfo struct {
	// Name is the prefix of its tools, mcp__<name>__<tool>.
	Name string
	// Placement is where it runs, and Where names that place: the
	// workspace, as a session's warnings name one, or the URL's origin
	// alone (urlPlace), since a hosted server's key is often in its
	// path. Where is empty on this machine.
	Placement Placement
	Where     string
	// Description is what the config, or the workspace that offers the
	// server, says it is for: text from whoever wrote it, which a front
	// cleans before it shows it.
	Description string
	// Secrets says this session gives it secrets (MCPServer.SecretEnv)
	// and starts it on this machine, redacting them from what it
	// writes.
	Secrets bool
	// HoldsSecrets says the workspace that offers it gives it secrets
	// there (agentworkspace's Server.Secrets): it informs the listing
	// alone, since a server not marked may still hold one.
	HoldsSecrets bool
	// Added is the label RemoveMCP takes, for a server AddMCP started;
	// empty for one the session opened with.
	Added string
	// State is where the server's session is when Info is read
	// (mcpclient's Remote.State): live, ended (its process exited or its
	// connection dropped; the next call to one of its tools starts it
	// again), restarting, failed (its last restart, with Failure, the
	// error; the next call tries again) or closed. Empty from a dax
	// that did not say.
	State   string `json:",omitempty"`
	Failure string `json:",omitempty"`
}

// DefaultUserDir is ~/.dax.
func DefaultUserDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".dax"
	}
	return filepath.Join(home, ".dax")
}

// DefaultRoot is ~/.dax/sessions.
func DefaultRoot() string { return filepath.Join(DefaultUserDir(), "sessions") }

// Session is one dax conversation: the kit that assembled it, the
// in-process human plane over it (Backend), dax's own controls
// (Controls), and the store it is written to.
type Session struct {
	Kit *agentkit.Kit

	// outs are the tool outputs it cut and kept (ToolOutput).
	outs keptOutputs
	// templates are the prompt templates it offers (Templates).
	templates []PromptTemplate
	// tasks are what a fold keeps beside the person's own messages.
	tasks tasks
	// forwards are the ports Forward put on this machine's loopback, by
	// the workspace's port; each ends with the session.
	fwdMu    sync.Mutex
	forwards map[int]net.Listener

	opts Options
	// base is the Options the session was opened with, as the caller
	// gave them, which Reopen opens another over.
	base   Options
	live   live
	modes  modes
	grants grants
	// grantRules are the extensions' GrantRule by tool (GrantOffer).
	grantRules map[string]func(context.Context, string, json.RawMessage) string
	// personOnly are the tools an A2A caller is refused
	// (personOnlyRefusal).
	personOnly map[string]bool
	env        []string // what an MCP server outside the workspace starts with
	offered    []string // the MCP servers the workspace offers, connected at open
	// beside are the servers workspaces, by name, each with its
	// descriptor and the MCP servers it offered, connected at open.
	beside []ServersBeside
	// reserved are the names no other server may take: the offered
	// servers' and a box host's units'.
	reserved []string
	mcpNames []string // the configured servers connected at open
	// added are the servers AddMCP started, by label, in the order they
	// were, which Reopen starts again.
	added   []addedMCP
	refused []agentkit.Omission // repository files screened out before the kit
	ws      workspace.Workspace
	ownWS   bool // the session opened ws, and closes it
	files   *tool.Files
	x       executor.Executor // runs the extensions' tools; the session closes it
	set     *executor.Set     // x's tools as the kit has them
	store   agentsession.Store
	own     bool // the session opened store, and closes it
	// recorded is false when the store is one the session made in
	// memory because nothing was to be kept.
	recorded bool

	ag     *agentturn.Agent  // the one agent
	ctl    *agentkit.Control // the kit's control of it, which the session's own delegates to
	detach func()
	// answering counts the fronts answering questions (Answering).
	answering atomic.Int32
	// skillsStale is set when an extension asked for its skills to be
	// discovered again (skillsTurn).
	skillsStale atomic.Bool
	// views counts the views attached to it (Viewing), which an A2A
	// caller's conversation's questions need.
	views views
	// host is this machine's directory as a workspace, over env, made
	// for the first secret-holding server that runs here (hostStarter);
	// the session closes it.
	host   *workspace.Local
	hostMu sync.Mutex
	// servers are the MCP servers connected, in the order they were,
	// as Info.MCP lists them.
	servers   []MCPServerInfo
	serversMu sync.Mutex
	// nestedMD gives the model the AGENTS.md files below the start
	// that the work reaches (nestedmd.go), main agent and sub-agents
	// alike; nil with AGENTS.md off.
	nestedMD func(context.Context, agentturn.TurnStartInfo) (openresponses.Items, error)
	// callerMD is the AGENTS.md part without the person's own files, as
	// an A2A caller's requests carry it (caller.go).
	callerMD callerMD
	// closing makes Close once, and closeErr is its error.
	closing  sync.Once
	closeErr error
}

// live is what /think and /model have set, read by each sub-agent
// call, which may run while the main agent's settings change.
type live struct {
	mu    sync.Mutex
	think bool
	main  string
}

// now is the Think setting and the main model in force.
func (s *Session) now() (think bool, main string) {
	s.live.mu.Lock()
	defer s.live.mu.Unlock()
	return s.live.think, s.live.main
}

// Describe is one line for the user: the call and why it is unanswered.
func Describe(p agentturn.PendingCall) string {
	s := fmt.Sprintf("%s(%s) %s", p.Call.Name, p.Call.Arguments, p.Reason)
	if p.Reason == agentturn.PendingDeferred && p.Dispatched {
		s += " after its dispatch"
	}
	return s
}

// withPinnedFacts is the session's adjustment to the agent's
// configuration. The facts of the model response's calls are held for
// their decisions (executor.Set.PinBatch), so the policy's subjects, the
// rewrite a call runs with, the decision about that rewrite and the
// batch hold's reading of each sibling come from one reading of the
// response, two requests to an executor elsewhere. What a deferred
// call's question is about travels on the decision (its Subject, from
// the verdict) and is recorded with the verdict, so the reason stays
// the policy's own.
func (s *Session) withPinnedFacts(cfg agentturn.Config) agentturn.Config {
	inner := cfg.BeforeToolCall
	if inner == nil {
		return cfg
	}
	set := s.set
	cfg.BeforeToolCall = func(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
		ctx, done := set.PinBatch(ctx, info)
		defer done()
		return inner(ctx, info)
	}
	return cfg
}

// withModeTurn is cfg with the session's skillsTurn and modeTurn after
// its BeforeTurn.
func (s *Session) withModeTurn(cfg agentturn.Config) agentturn.Config {
	cfg.BeforeTurn = agentturn.ChainBeforeTurn(cfg.BeforeTurn, s.skillsTurn, s.modeTurn)
	return cfg
}

// skillsTurn reloads the kit's skills before the turn's model call when
// an extension asked for it (extension.Env.ReloadSkills): at a turn's
// start no call of the agent is being decided, the last batch done, so
// no call is decided on one catalogue's claim and served from the
// other's, which a reload in the middle of a batch would allow
// (agentkit's Kit.ReloadSkills). A failed reload keeps the catalogue
// in force, with a warning.
//
// The gap it leaves: an A2A caller's conversation (PeerConfig) runs on
// the same kit at the same time as the main agent, and nothing here
// orders its batches against the main agent's turns, so a caller's
// batch can be decided on one catalogue and served from the next. A
// reload follows only the person's own write (skill_manage is refused
// to a caller, extension.Extension.PersonOnly), and a caller reads
// only installed skills (dax-skills' callerReads); what can
// straddle it is a learned skill's listing, never its body, and an
// installed skill as its directory stood when the reload read it.
func (s *Session) skillsTurn(ctx context.Context, _ agentturn.TurnStartInfo) (openresponses.Items, error) {
	if !s.skillsStale.Swap(false) {
		return nil, nil
	}
	if err := s.Kit.ReloadSkills(ctx); err != nil {
		fmt.Fprintf(warnings{}, "dax: skills: %v\n", err)
	}
	return nil, nil
}

// PartOneShot is the id of the instructions part a one-shot run
// (Options.OneShot) adds.
const PartOneShot = "one-shot"

// oneShotLine is what a one-shot run's model is told.
const oneShotLine = "This is a one-shot run: nobody will reply to your last message, and the run ends with it. " +
	"Act on your best judgement and finish the task rather than ending with a question or a list of options; " +
	"say what you chose and why in your final report."

// answerOf is the output the model is given for an unanswered call: a
// call whose tool returned before a stop is owed what it returned
// (#162), and every other call what is known of it.
func answerOf(p agentturn.PendingCall) *openresponses.FunctionCallOutput {
	if p.Reason == agentturn.PendingReturned && p.Ran != nil {
		out := *p.Ran
		out.CallID = p.Call.CallID
		return &out
	}
	return openresponses.NewFunctionCallOutput(p.Call.CallID, output(p))
}

// output is what the model is told about an unanswered call.
func output(p agentturn.PendingCall) string {
	switch p.Reason {
	case agentturn.PendingDeferred:
		if p.Dispatched {
			return heldOutput
		}
		return deniedOutput
	case agentturn.PendingAborted, agentturn.PendingAnswered:
		return abortedOutput
	case agentturn.PendingUndispatched:
		return neverRanOutput
	}
	return unknownOutput
}

func (o Options) log(format string, args ...any) {
	if o.Log != nil {
		o.Log(format, args...)
	}
}

// reasoningFor is the reasoning Think implies, fitted to model.
func (o Options) reasoningFor(ctx context.Context, model string) openresponses.ReasoningConfig {
	return o.fit(ctx, model, o.reasoning())
}

// fit fits r's effort to model; an effort fitted to none asks for no
// summary.
func (o Options) fit(ctx context.Context, model string, r openresponses.ReasoningConfig) openresponses.ReasoningConfig {
	if o.Fit == nil || r.Effort == "" {
		return r
	}
	if e := o.Fit(ctx, model, r.Effort); e != r.Effort {
		r.Effort = e
		if e == openresponses.ReasoningEffortNone {
			r.Summary = ""
		}
	}
	return r
}

func (o Options) reasoning() openresponses.ReasoningConfig {
	if o.Think {
		e := o.Effort
		if e == "" {
			e = openresponses.ReasoningEffortLow
		}
		return openresponses.ReasoningConfig{
			Effort:  e,
			Summary: openresponses.ReasoningSummaryAuto,
		}
	}
	return openresponses.ReasoningConfig{Effort: openresponses.ReasoningEffortNone}
}

// open assembles the kit and the agent. resume, when not empty, is the
// session to continue; the store is nil when nothing is recorded.
//
// The extensions are built in two phases: every extension's tools, then
// each one's kit options, over an Env that sees all the tools. The
// session's own options go to the kit after the extensions', so where
// an option replaces (the name, the model, the instructions, the
// policy, the session) the session's is the one in force.
func open(ctx context.Context, o Options, store agentsession.Store, own bool, resume string) (*Session, error) {
	if o.Streamer == nil {
		return nil, errors.New("no model")
	}
	base := o
	ws, ownWS := o.Workspace, false
	if o.Executor != nil {
		if ws != nil {
			return nil, errors.New("both an executor and a workspace: the executor's tools act in its own")
		}
		ws = o.Executor.Workspace()
		// The project's files are read through the executor. One
		// whose files cannot be read (gone, or too slow) fails the
		// session here, with that said, rather than as one part of
		// the prompt or another.
		if _, err := fs.Stat(ws.FS(), "."); err != nil {
			return nil, fmt.Errorf("the executor's workspace cannot be read: %w", err)
		}
	}
	if err := checkSecretEnv(o); err != nil {
		return nil, err
	}
	if ws == nil {
		local, err := workspace.NewLocal(o.Dir, tool.DefaultEnv(o.PassEnv, o.secrets()...))
		if err != nil {
			return nil, fmt.Errorf("workspace: %w", err)
		}
		ws, ownWS = local, true
	}
	switch {
	case o.Name == "":
		o.Name, o.Version = "dax", Version
		if o.Description == "" {
			o.Description = "A coding agent that reads, writes and edits files and runs shell commands in a project."
		}
	case o.Description == "":
		o.Description = "An agent built on dax."
	}
	if o.MaxTurns < 0 || o.MaxTokens < 0 || o.MaxCost < 0 {
		return nil, errors.New("a limit on turns, tokens or cost is zero or more")
	}
	// Every model call the session makes, a sub-agent's or a fold's
	// summary among them, goes through the meter.
	m, err := newMeter(o)
	if err != nil {
		return nil, err
	}
	if r, ok := store.(agentsession.Reader); ok && m != nil && resume != "" {
		// A resumed session goes on from what its record says it spent.
		m.seed(ctx, r, resume, o.Model)
	}
	o.Streamer, o.SubagentStreamer = m.wrap(o.Streamer), m.wrap(o.SubagentStreamer)
	// A read's and the person's images reach only a model that takes
	// them, each request judged by the model it names: the session's
	// requests in its BeforeModelCall, so the record holds what was
	// sent; a fold's summary and a server's compaction, which no hook
	// sees, here (images.go).
	o.Streamer, o.SubagentStreamer = textOnly(o.TakesImages, o.Streamer), textOnly(o.TakesImages, o.SubagentStreamer)
	s := &Session{opts: o, base: base, store: store, own: own, ws: ws, ownWS: ownWS, files: tool.NewFiles(ws)}
	// A servers-only workspace's project config was not read either
	// (the program reading it leaves it out the same way); say so, as
	// for its AGENTS.md.
	if config.ServersAlone(ws) {
		s.refused = append(s.refused, omission("config", path.Join(ws.Root(), config.ProjectName), noRead))
	}
	s.live.think, s.live.main = o.Think, o.Model
	// The user's own templates are of the home directory a bare run
	// leaves out; the project's are part of its task.
	userDir := o.UserDir
	if o.Bare != nil {
		userDir = ""
	}
	var wsFS fs.FS
	if !config.ServersAlone(ws) {
		wsFS = ws.FS()
	}
	s.templates = loadTemplates(userDir, wsFS, func(msg string) { fmt.Fprintf(warnings{}, "dax: %s\n", msg) })
	ok := false
	defer func() {
		if !ok {
			s.closeTools()
			s.closeHost()
			if ownWS {
				ws.Close()
			}
		}
	}()
	model := o.Streamer
	var engine atomic.Pointer[agentpolicy.Engine]
	// An MCP server runs in the workspace when it can start one, and
	// otherwise on this machine, with this machine's environment
	// scrubbed (mcpServer).
	env := tool.DefaultEnv(o.PassEnv, o.secrets()...)
	s.env = env
	te := extension.ToolEnv{Workspace: ws, Files: s.files, MaxReadBytes: o.MaxReadBytes}
	// The extensions' tools run in the executor; the session decides
	// on their calls and runs them through it.
	s.x = o.executor
	if s.x == nil && o.Executor != nil {
		if err := o.Executor.runs(o.Extensions); err != nil {
			return nil, err
		}
		s.x = keepOpen{o.Executor.r}
	}
	if s.x == nil {
		x, err := executor.InProcess(o.Extensions, te)
		if err != nil {
			return nil, err
		}
		s.x = x
	}
	a, err := buildFrom(ctx, o.Extensions, te, s.x, o.Executor != nil)
	if err != nil {
		return nil, err
	}
	s.set = a.set
	s.grantRules = a.grantRules
	s.personOnly = a.personOnly

	// agentsText is the AGENTS.md part as the kit renders it, for a
	// sub-agent whose configuration is fixed before the kit is built.
	// The kit has no instruction budget, under which it renders the
	// chain whole, so the same options give the same text.
	var kopts []agentkit.Option
	agentsText := ""
	if o.AgentsMD {
		// The chain is read through the workspace, screened first so
		// that a file that is a link out of it is left out and
		// reported rather than failing the session (trust.go). With
		// an executor it is the executor's workspace, read through it,
		// and no directory of this machine is the workspace's,
		// whatever its descriptor says (an executor of kind local may
		// be on another host), so no file above it is read from here.
		dir := o.Dir
		if o.Executor != nil {
			dir = ""
		}
		userDir := o.UserDir
		if o.NoUserAgentsMD {
			userDir = ""
		}
		mdOpts, personal, refused := agentsMDOptions(ws, dir, userDir, o.AgentsMDGlobal)
		s.refused = append(s.refused, refused...)
		kopts = append(kopts, agentkit.WithAgentsMD(agentsMDPath, mdOpts))
		res, err := agentsmd.Chain(agentsMDPath, mdOpts)
		if err != nil {
			return nil, fmt.Errorf("AGENTS.md: %w", err)
		}
		agentsText = agentsmd.Render(res.Files)
		// The person's own files are not an A2A caller's (caller.go).
		s.callerMD = newCallerMD(res.Files, personal)
		// The files below the start directory reach the model as the
		// work reaches their directories (nestedmd.go).
		var start []string
		for _, f := range res.Files {
			start = append(start, f.Path)
		}
		// One hook for the main agent and its sub-agents (ChildBeforeTurn),
		// which shares what it read of each bash line.
		s.nestedMD = nestedAgentsMD(s.files, mdOpts, start, tool.BashSubjects(s.files, o.MaxReadBytes))
		kopts = append(kopts, agentkit.WithBeforeTurn(s.nestedMD))
	}
	kopts = append(kopts, agentkit.WithTools(a.tools...))
	// An A2A caller's request leaves out what is the person's own
	// (caller.go); the person's go as the kit makes them. A request's
	// images are said to a model that does not take them here, before
	// the recorder takes it (images.go).
	kopts = append(kopts, agentkit.WithBeforeModelCall(s.beforeModelCall))
	// Every tool's result is cut to its cap, an MCP server's and a
	// child agent's among them, in every placement (outputcap.go).
	// A read's image is shown only to a model that takes images (images.go).
	kopts = append(kopts, agentkit.WithToolWrap(func(_ string, t agenttool.Tool) agenttool.Tool { return s.capTool(s.imageGate(t)) }))
	xenv := &sessionEnv{s: s, a: a, te: te, agentsMD: agentsText, eng: &engine}
	if err := xenv.buildSubagentTools(o.Extensions); err != nil {
		return nil, err
	}
	for _, e := range o.Extensions {
		if e.Kit == nil {
			continue
		}
		eo, err := e.Kit(xenv)
		if err != nil {
			return nil, fmt.Errorf("extension %s: %w", e.Name, err)
		}
		kopts = append(kopts, eo...)
	}

	kopts = append(kopts,
		agentkit.WithName(o.Name, o.Description),
		agentkit.WithModel(model, o.Model),
		agentkit.WithReasoning(o.reasoningFor(ctx, o.Model)),
		agentkit.WithRetry(agentturn.Retry{MaxAttempts: 3}),
		agentkit.WithInstructions(s.systemPrompt()),
	)
	if o.MaxTurns > 0 {
		kopts = append(kopts, agentkit.WithMaxTurns(o.MaxTurns))
	}
	if o.OneShot {
		kopts = append(kopts, agentkit.WithPart(PartOneShot, "dax", func(context.Context) (agentkit.PartRender, error) {
			return agentkit.PartRender{Text: oneShotLine}, nil
		}))
	}
	if m != nil {
		kopts = append(kopts, agentkit.WithBeforeTurn(m.beforeTurn))
	}
	// The servers the workspace offers of its own, a box host's units
	// among them, each connected below as the config's are. Behind a box
	// host, every MCP unit its handshake names must be one: an older box
	// host passes servers.list through to the executor, which offers
	// none, so its units would not start, and a server of another kind
	// could take a unit's name and the confinement box.confine claims
	// for it.
	var offered []MCPServer
	if sp, ok := ws.(workspace.ServerProvider); ok {
		list, err := offeredServers(ctx, sp, "the workspace", PlacedOffered, where(ws.Descriptor()))
		if err != nil {
			return nil, err
		}
		for _, m := range list {
			s.offered = append(s.offered, m.Name)
		}
		offered = append(offered, list...)
	}
	s.reserved = slices.Clone(s.offered)
	if o.Executor != nil {
		for _, u := range boxUnits(o.Executor.r.Box()) {
			if !slices.Contains(s.offered, u) {
				return nil, fmt.Errorf("the box host offers no server for unit %s; it is older than this dax, update it", u)
			}
			if !slices.Contains(s.reserved, u) {
				s.reserved = append(s.reserved, u)
			}
		}
	}
	// The servers workspace's, beside them under the same rules: a name
	// the workspace offers, or a box host's unit, is refused, since the
	// two servers' tools would be one name and box.confine claims a
	// unit's by its name.
	besides, err := serversWorkspaces(o)
	if err != nil {
		return nil, err
	}
	// Who offered each reserved name, for a collision's message.
	by := map[string]string{}
	for _, r := range s.reserved {
		by[r] = "the workspace"
	}
	for _, b := range besides {
		sp, err := checkServersWorkspace(b.ws, ws)
		if err != nil {
			return nil, err
		}
		d := b.ws.Descriptor()
		this := "the servers workspace " + where(d)
		if b.name != config.ServersName || len(besides) > 1 {
			this = "the servers workspace " + b.name + " (" + where(d) + ")"
		}
		for _, o := range s.beside {
			if o.Workspace.Kind == d.Kind && o.Workspace.Ref == d.Ref {
				return nil, fmt.Errorf("%s is the servers workspace %s too; name each once", this, o.Name)
			}
		}
		list, err := offeredServers(ctx, sp, this, PlacedServersWorkspace, where(d))
		if err != nil {
			return nil, err
		}
		rec := ServersBeside{Name: b.name, Workspace: workspace.Descriptor{Kind: d.Kind, Ref: d.Ref, Root: d.Root}, Servers: []string{}}
		for _, m := range list {
			if i := slices.IndexFunc(s.reserved, func(r string) bool { return config.MCPNamesCollide(r, m.Name) }); i >= 0 {
				return nil, fmt.Errorf("mcp %s: %s and %s both offer a server by that name; offer it from one", m.Name, by[s.reserved[i]], this)
			}
			rec.Servers = append(rec.Servers, m.Name)
		}
		for _, n := range rec.Servers {
			s.reserved = append(s.reserved, n)
			by[n] = this
		}
		s.beside = append(s.beside, rec)
		offered = append(offered, list...)
	}
	if o.Policy != nil {
		settings := *o.Policy
		settings.Shipped = append(slices.Clone(settings.Shipped), a.shipped...)
		// The rules the user kept for this project ("yes, always").
		if f := o.grantsFile(); f != "" {
			settings.SavedPath = f
			saved, err := readGrants(f, ws.Descriptor())
			if err != nil {
				fmt.Fprintf(warnings{}, "dax: %v\n", err)
			}
			settings.Saved = append(slices.Clone(settings.Saved), saved...)
		}
		// box.confine: the box's tools the policy takes as confined, and
		// the rules that make that matter (confine.go).
		confine, err := confineBox(o, a.set, &settings, a.matchers, a.aliases, s.offered)
		if err != nil {
			return nil, err
		}
		p, err := policy.Build(settings)
		if err != nil {
			return nil, err
		}
		// The settings stay with the session, so a mode switch
		// rebuilds the same rules (SetMode).
		s.modes.settings, s.modes.mode = &settings, settings.Mode
		if s.modes.mode == "" {
			s.modes.mode = policy.ModeDefault
		}
		s.modes.inForce.Store(s.modes.mode)
		// The A2A callers' floor: the same rules in the default mode,
		// which a call of theirs is held to (peerFloor).
		pp, err := peerPolicy(settings)
		if err != nil {
			return nil, err
		}
		lookup := agentpolicy.WithToolsFor(func(ctx context.Context, name string) (agenttool.Tool, bool) { return s.Kit.LookupToolFor(ctx, name) })
		if s.modes.peers, err = agentpolicy.Build(pp, a.matchers, append([]agentpolicy.Option{agentpolicy.WithAliases(a.aliases), lookup}, confine...)...); err != nil {
			return nil, err
		}
		kopts = append(kopts, agentkit.WithPolicy(p, a.matchers, append([]agentpolicy.Option{agentpolicy.WithAliases(a.aliases), agentpolicy.WithHooks(s.peerFloor, s.personOnlyRefusal, s.dontAskRefusal)}, confine...)...))
		if h := a.hook(); h != nil {
			kopts = append(kopts, agentkit.WithBeforeToolCall(h))
		}
	} else {
		// With the policy off nothing is asked, and a caller is still
		// refused what is the person's own.
		kopts = append(kopts, agentkit.WithBeforeToolCall(s.personOnlyRefusal))
	}
	// A tool's question mid-call is a question event to the fronts
	// answering, past the session's rules (elicit).
	kopts = append(kopts, agentkit.WithToolElicitor(agentpolicy.ByHuman, s.elicit))
	// Set whether or not a server is configured, since /mcp may add one.
	kopts = append(kopts, agentkit.WithMCPStderr(os.Stderr))
	_, starts := ws.(workspace.Starter)
	servers := trusted(o.MCP, ws.Descriptor(), !starts, o.trustFile(), o.TrustMCP)
	// Two servers whose tools would be named alike are refused: a rule
	// for one's tools would be the other's (the -mcp flag's "cli"
	// beside a configured "cli" among them).
	for i, a := range servers {
		for _, b := range servers[i+1:] {
			if config.MCPNamesCollide(a.Name, b.Name) {
				return nil, fmt.Errorf("mcp %s and %s: their tools would be named alike; rename one", a.Name, b.Name)
			}
		}
	}
	// A configured server whose tools would be named as a reserved one's
	// (one the workspace offers, or a box host's unit) is refused: their
	// tools' names would be one, and box.confine claims a unit's tools by
	// their name.
	for _, r := range s.reserved {
		if slices.ContainsFunc(servers, func(m MCPServer) bool { return collidesWithOffered(m.Name, r) }) {
			return nil, fmt.Errorf("mcp %s: a server your config names and one the workspace offers have the name; rename the server", r)
		}
	}
	for _, m := range servers {
		s.mcpNames = append(s.mcpNames, m.Name)
	}
	for _, m := range servers {
		t, mopts, info, err := s.mcpStart(ctx, m)
		if err != nil {
			return nil, fmt.Errorf("mcp %s: %w", m.Name, err)
		}
		kopts = append(kopts, agentkit.WithMCPTransport(t, mopts...))
		s.servers = append(s.servers, info)
	}
	// The offered servers are connected as the configured ones are,
	// through mcpStart, so a server's tools, their names, its restarts,
	// how its output reaches the session and how Info lists it are one
	// wherever it runs.
	for _, m := range offered {
		t, mopts, info, err := s.mcpStart(ctx, m)
		if err != nil {
			return nil, fmt.Errorf("mcp %s: %w", m.Name, err)
		}
		kopts = append(kopts, agentkit.WithMCPTransport(t, mopts...))
		s.servers = append(s.servers, info)
	}
	if o.Compact > 0 {
		fold := agentkit.WithFoldObserver(func(_ context.Context, f compact.Fold) {
			switch {
			case errors.Is(f.Err, compact.ErrSummaryTooLarge), errors.Is(f.Err, compact.ErrSummaryIncomplete), errors.Is(f.Err, compact.ErrSummaryNoText):
				// The fold is given up and the transcript sent whole;
				// compaction does not ask about this prefix again until
				// it has grown.
				o.log("[not compacted: %v; %d tokens sent unfolded]", f.Err, f.TokensBefore)
			case f.Err != nil:
				o.log("[compaction failed after %d call(s): %v]", f.Attempts, f.Err)
			case f.Summary != nil && f.Forced:
				o.log("[compacted on request: %d item(s) folded, %d tokens before]", f.Split, f.TokensBefore)
			case f.Summary != nil:
				o.log("[compacted: %d item(s) folded, %d tokens before]", f.Split, f.TokensBefore)
			}
		})
		if o.CompactServer {
			c, ok := model.(compact.Compactor)
			if !ok {
				return nil, errors.New("-compact-server: this provider has no compaction endpoint")
			}
			kopts = append(kopts, agentkit.WithCompactor(c, o.Compact, compact.WithPin(s.tasks.pin)), fold)
		} else {
			// The kit asks the summary under the agent's reasoning, which
			// under -think is effort low. qwen3.5 then spends most of
			// the summary's output cap reasoning: of three folds of one
			// transcript, two were asked twice, the summaries were 106
			// to 649 bytes against 858 to 1,313 at effort none, and they
			// took 3.4 times as long. So the summary stays at none.
			// Fitted to the model the summary is asked of, which may be
			// one that always reasons.
			none := compact.WithRequest(func(r *openresponses.Request) {
				r.Reasoning = o.fit(context.Background(), r.Model, openresponses.ReasoningConfig{Effort: openresponses.ReasoningEffortNone})
			})
			// The person's own messages are kept through every fold
			// (agentturn's compact default), and an autonomous run's
			// task beside them (tasks), each recorded in the compaction
			// entry.
			kopts = append(kopts, agentkit.WithCompaction(o.Compact, none, compact.WithPin(s.tasks.pin)), fold)
		}
	}
	if store != nil {
		if resume != "" {
			kopts = append(kopts, agentkit.WithResumedSession(store, resume, s.envOption()))
		} else {
			kopts = append(kopts, agentkit.WithSession(store, s.header(), s.envOption()))
		}
	}

	kit, err := agentkit.New(ctx, kopts...)
	if err != nil {
		return nil, err
	}
	s.Kit = kit
	if err := a.bind(kit); err != nil {
		kit.Close()
		return nil, err
	}
	if e := kit.Engine(); e != nil {
		if o.Policy == nil {
			// The session's policy would have replaced it; with none,
			// an extension's would decide calls the user turned the
			// policy off for.
			kit.Close()
			return nil, errors.New("an extension set a policy on a session with the policy off")
		}
		engine.Store(e)
	}
	// The session as the store holds it now, not New's reading: an
	// agent seeded from an older reading would hold an item the record's
	// omit leaves out since, a quarantine's, and send it again.
	seed, err := kit.ReadAgentOptions(ctx)
	if err != nil {
		kit.Close()
		return nil, err
	}
	s.ag = agentturn.New(s.withModeTurn(s.withPinnedFacts(kit.Config())), seed...)
	// A fold keeps the record's unclaimed prompts too: an older
	// record's typed lines, -p's prompt.
	s.claimUnclaimed()
	// The kit's recorder subscribes first, so an entry is in the store
	// before any front sees the event that wrote it.
	s.detach = kit.Attach(s.ag)
	s.ag.Subscribe((&truncationWatch{log: o.log}).handle)
	// A mode asked for during a run starts at its next turn.
	s.ag.Subscribe(s.modeAtTurn)
	if s.modes.settings != nil && s.modes.mode != policy.ModeDefault {
		s.annotateMode(ctx, modeEntry{Mode: s.modes.mode, Reason: "start"})
	}
	// The kit's control hands the agent the inputs a stopped session
	// accepted and no run took, which it takes after the next prompt.
	// The session's elicitor is the kit's own, so the control leaves
	// the agent's config as it is.
	if s.ctl, err = kit.Control(s.ag); err != nil {
		return nil, err
	}
	ok = true
	return s, nil
}

// header names the harness and the cwd: the workspace's root, the
// path the tools act in, on whichever machine that is; and in dax's
// own members, the workspace whole and the executor's address, hashed
// (extras), which say which machine.
func (s *Session) header() agentsession.Header {
	return agentsession.Header{
		// Media, an image a read returned or the person attached, is
		// kept as the store's blobs beside the log (images.go).
		Media:   agentsession.MediaSidecar,
		CWD:     s.ws.Root(),
		Harness: &agentsession.Harness{Name: s.opts.Name, Version: s.opts.Version},
		Extra:   s.extras(),
	}
}

// envOption is the environment entry the recorder asks for once per
// run: the directory the tools are rooted at and which file system it
// is in, from the workspace's descriptor (this machine, a container, a
// remote runtime). The recorder writes it only when it differs from the
// last one on the path, so a session that stays in one workspace holds
// one entry and a resume in another says so.
func (s *Session) envOption() session.Option {
	x := s.x
	return session.WithEnv(func(context.Context) (*agentsession.EnvEntry, error) {
		d := x.Descriptor()
		e := agentsession.NewEnvEntry(d.Root)
		// The machine and the file system's instance too, which the
		// substitution rule compares (agentworkspace RFC 0001 §12).
		w := e.SetWorkspace(d.Kind, d.Ref)
		w.Host, w.Instance = d.Host, d.Instance
		return e, nil
	})
}

// New starts a fresh session.
func New(ctx context.Context, o Options) (*Session, error) {
	store, own, err := o.openStore()
	if err != nil {
		return nil, err
	}
	recorded := store != nil
	if !recorded {
		store, own = memoryStore(), true
	}
	s, err := open(ctx, o, store, own, "")
	if s != nil {
		s.recorded = recorded
	}
	if err != nil {
		if c, ok := store.(io.Closer); ok && own {
			c.Close()
		}
		return nil, err
	}
	return s, nil
}

// openStore is the store the session records into: the caller's, or
// the one at Root, which the session owns; nil when neither is set.
func (o Options) openStore() (store agentsession.Store, own bool, err error) {
	switch {
	case o.Store != nil:
		return o.Store, false, nil
	case o.Root != "":
		cs, err := openStore(o.Root, cas.WithSync(o.Sync))
		if err != nil {
			return nil, false, err
		}
		return cs, true, nil
	}
	return nil, false, nil
}

// memoryStore is the store of a session nothing is kept of: the
// backend follows a record, so there is one, in memory, gone at Close.
func memoryStore() agentsession.Store { return agentsession.NewMemoryStore() }

// Resume reopens a recorded session and continues it from its leaf.
// Calls an earlier run left unanswered become pending on the agent,
// each with the reason the file's dispatch records give it, and are
// answered by the next Prompt.
func Resume(ctx context.Context, o Options, id string) (*Session, error) {
	store, own, err := o.openStore()
	if err != nil {
		return nil, err
	}
	if store == nil {
		return nil, errors.New("resume needs a session store")
	}
	s, err := open(ctx, o, store, own, id)
	if s != nil {
		s.recorded = true
	}
	if err != nil {
		if c, ok := store.(io.Closer); ok && own {
			c.Close()
		}
		return nil, err
	}
	if err := s.resumed(id); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// resumed checks a session open has just resumed as id, and warns of
// what changed since it was recorded: a session that cannot be
// continued is an error, and the caller closes it.
func (s *Session) resumed(id string) error {
	sess := s.Kit.Session()
	if t := sess.Truncated(); t != nil {
		fmt.Fprintf(warnings{}, "dax: session %s: %v (dropped)\n", id, t)
	}
	// A session whose header or entries hold a redaction's members from
	// before 0.13 is never raised (agentsession.ErrNoRaise), so nothing
	// can be appended to it: it is refused here, rather than by the
	// first prompt's append.
	if err := sess.RaiseRefused(agentsession.Format); err != nil {
		return fmt.Errorf("session %s cannot be continued: %w", id, err)
	}
	if f := sess.DeclaredFormat(); f != agentsession.Format {
		// The recorder raises the header before its first append, after
		// which a reader that knows only earlier formats refuses it.
		fmt.Fprintf(warnings{}, "dax: session %s was written as %s; this dax writes %s, and readers that know only earlier formats refuse it from the next entry\n", id, f, agentsession.Format)
	}
	// The workspace the header records against the one the session
	// continues in: a session that ran in a container and continues
	// here, or the other way, is said to, even when the two roots are
	// the same path. One recorded before dax wrote its workspace says
	// only its cwd, which is compared with the root as before.
	now := s.identity()
	if was, ok := recordedIn(sess.Header()); ok && !sameWorkspace(was, now) {
		// Named by host and instance only when they are the change.
		name := where
		if sameProject(was, now) {
			name = placed
		}
		fmt.Fprintf(warnings{}, "dax: session was recorded in %s, continuing in %s\n", name(was), name(now))
	} else if cwd := sess.Header().CWD; !ok && cwd != "" && cwd != now.Root {
		fmt.Fprintf(warnings{}, "dax: session was recorded in %s, continuing in %s\n", cwd, where(now))
	}
	// So is the servers workspace: the header keeps the one the session
	// started with, which -list names.
	if w := serversResumed(sess.Header(), s.beside); w != "" {
		fmt.Fprintf(warnings{}, "dax: %s\n", w)
	}
	return nil
}

// serversResumed is what to say when a session whose header h records
// its servers workspaces (none, one, or several by name) continues
// with others (now); "" when they are the same.
func serversResumed(h agentsession.Header, now []ServersBeside) string {
	name := func(d workspace.Descriptor) string { return d.Kind + " " + d.Ref }
	was := ServersWorkspacesOf(h)
	single := func(l []ServersBeside) bool {
		return len(l) == 0 || len(l) == 1 && l[0].Name == config.ServersName
	}
	if single(was) && single(now) {
		// One servers workspace, or none, either side: said as before.
		switch {
		case len(was) == 1 && len(now) == 0:
			return "session's servers were recorded from servers workspace " + name(was[0].Workspace) + ", continuing without one"
		case len(was) == 0 && len(now) == 1:
			return "session was recorded without a servers workspace, continuing with " + name(now[0].Workspace)
		case len(was) == 1 && len(now) == 1 && (was[0].Workspace.Kind != now[0].Workspace.Kind || was[0].Workspace.Ref != now[0].Workspace.Ref):
			return "session's servers were recorded from servers workspace " + name(was[0].Workspace) + ", continuing with " + name(now[0].Workspace)
		}
		return ""
	}
	list := func(l []ServersBeside) string {
		if len(l) == 0 {
			return "none"
		}
		var parts []string
		for _, b := range l {
			parts = append(parts, b.Name+" "+name(b.Workspace))
		}
		return strings.Join(parts, ", ")
	}
	if list(was) == list(now) {
		return ""
	}
	return "session's servers were recorded from servers workspaces " + list(was) + ", continuing with " + list(now)
}

// namedWorkspace is a servers workspace the session connects and its
// name.
type namedWorkspace struct {
	name string
	ws   workspace.Workspace
}

// serversWorkspaces are o's servers workspaces, by name: ServersWorkspace
// as config.ServersName, then ServersWorkspaces in the order of their
// names, each name a DNS label.
func serversWorkspaces(o Options) ([]namedWorkspace, error) {
	var out []namedWorkspace
	if o.ServersWorkspace != nil {
		out = append(out, namedWorkspace{config.ServersName, o.ServersWorkspace})
	}
	names := slices.Sorted(maps.Keys(o.ServersWorkspaces))
	for _, n := range names {
		switch {
		case !serversName.MatchString(n):
			return nil, fmt.Errorf("servers workspace %q: a name is a DNS label of lower-case letters, digits and hyphens", n)
		case n == config.ServersName && o.ServersWorkspace != nil:
			return nil, fmt.Errorf("servers workspace %s: the name is ServersWorkspace's", n)
		case o.ServersWorkspaces[n] == nil:
			continue
		}
		out = append(out, namedWorkspace{n, o.ServersWorkspaces[n]})
	}
	return out, nil
}

// serversName is a servers workspace's name: a DNS label.
var serversName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ID is the session ID, empty when not recording.
func (s *Session) ID() string { return s.Kit.SessionID() }

// Path is the session's directory in the local store, its header, log
// and head; empty when not recording, or recording into a store that is
// not a local one.
func (s *Session) Path() string {
	cs, ok := s.store.(*cas.Store)
	if !ok {
		return ""
	}
	return filepath.Join(cs.Root(), "sessions", s.ID())
}

// Omitted is what the instruction layers considered and left out. A
// file of the AGENTS.md chain the kit left out is named by its path in
// the workspace's root, as the screening names one, where the kit
// names it within the workspace's file system.
func (s *Session) Omitted() []agentkit.Omission {
	out := append([]agentkit.Omission(nil), s.refused...)
	for _, om := range s.Kit.Omitted() {
		if om.Source == agentkit.SourceAgentsMD {
			om.What, om.By = s.inRoot(om.What), s.inRoot(om.By)
		}
		out = append(out, om)
	}
	return out
}

// inRoot is a name in the workspace's file system as a path in its
// root; an absolute path, such as the user's own file, or nothing, is
// returned as it is.
func (s *Session) inRoot(name string) string {
	if name == "" || path.IsAbs(name) {
		return name
	}
	return path.Join(s.ws.Root(), name)
}

// Tools lists the tools the kit assembled, each with its source.
func (s *Session) Tools() []agentkit.ToolOrigin { return s.Kit.Tools() }

// held reports a call held for approval before its dispatch: it never
// ran, and a person may still allow it.
func held(p agentturn.PendingCall) bool {
	return p.Reason == agentturn.PendingDeferred && !p.Dispatched
}

// SetModel changes the model name for later runs, with the reasoning
// fitted to it. The loop leaves the reasoning items another model
// produced out of the new model's requests, so a switch under -think
// sends no signature the new model refuses; the recorder writes those
// requests' responses unhashed.
//
// The sub-agents follow it from their next call: a task with model
// main runs it, and so does every sub-agent when no sub-agent model is
// configured.
func (s *Session) SetModel(name string) error {
	think, _ := s.now()
	o := s.opts
	o.Think = think
	a := s.ag
	cfg := a.Config()
	cfg.ModelName = name
	cfg.Reasoning = o.reasoningFor(context.Background(), name)
	if err := a.SetConfig(cfg); err != nil {
		return err
	}
	s.live.mu.Lock()
	s.live.main = name
	s.live.mu.Unlock()
	return nil
}

// Model is the model name later runs go to, as /model last set it: the
// name the requests carry, which a status line names.
func (s *Session) Model() string {
	_, m := s.now()
	return m
}

// Git reads the workspace's repository for a status line: its branch
// and whether the tree is dirty (tool.GitHead), wherever the tools act.
func (s *Session) Git(ctx context.Context) (tool.Git, error) { return tool.GitHead(ctx, s.files) }

// mcpPrefix is the prefix of a server's tools, mcp__<name>__<tool>
// once the client adds its separator; the policy's mcp__* rules and
// aliases name servers this way, whether the server came from the
// config or from /mcp add.
func mcpPrefix(name string) (string, error) {
	if err := config.CheckMCPName(name); err != nil {
		return "", err
	}
	return "mcp__" + name, nil
}

// AddMCP starts a stdio MCP server mid-session and offers its tools
// from the next run as mcp__<name>__<tool>. It returns the label
// RemoveMCP takes.
func (s *Session) AddMCP(ctx context.Context, name, command string) (string, error) {
	// A server whose tools would be named as an offered server's or a
	// unit's is refused, since box.confine claims a unit's tools by their
	// name, and so is one named as a configured server's.
	for _, u := range s.reserved {
		if collidesWithOffered(name, u) {
			return "", fmt.Errorf("mcp %s: the workspace's server %s offers tools under that name; pick another", name, u)
		}
	}
	for _, m := range s.mcpNames {
		if config.MCPNamesCollide(name, m) {
			return "", fmt.Errorf("mcp %s: the configured server %s offers tools under that name; pick another", name, m)
		}
	}
	t, mopts, info, err := s.mcpStart(ctx, MCPServer{Name: name, Command: command})
	if err != nil {
		return "", err
	}
	label, err := s.Kit.AddMCPTransport(ctx, t, mopts...)
	if err != nil {
		return "", err
	}
	info.Added = label
	s.serversMu.Lock()
	s.servers = append(s.servers, info)
	s.added = append(s.added, addedMCP{label: label, server: MCPServer{Name: name, Command: command}})
	s.serversMu.Unlock()
	return label, nil
}

// RemoveMCP closes a server AddMCP started; its tools are not offered
// from the next run, and Info no longer lists it. The kit drops a
// server it added before it closes it, so a close that fails still
// leaves it unlisted.
func (s *Session) RemoveMCP(label string) error {
	err := s.Kit.RemoveMCP(label)
	s.serversMu.Lock()
	s.servers = slices.DeleteFunc(s.servers, func(m MCPServerInfo) bool { return m.Added == label })
	s.added = slices.DeleteFunc(s.added, func(a addedMCP) bool { return a.label == label })
	s.serversMu.Unlock()
	return err
}

// mcpServers are the MCP servers connected, as Info.MCP lists them:
// never nil, so a session with none says so (an empty list in the
// served info), where a nil list is a session that does not list them.
func (s *Session) mcpServers() []MCPServerInfo {
	s.serversMu.Lock()
	out := append([]MCPServerInfo{}, s.servers...)
	s.serversMu.Unlock()
	if s.Kit == nil {
		return out
	}
	// Each server's remote is the kit's, found by the prefix it names
	// its tools under, which no two servers share.
	remotes := map[string]*mcpclient.Remote{}
	for _, m := range s.Kit.MCPServers() {
		remotes[m.Remote.Name("")] = m.Remote
	}
	for i := range out {
		r, ok := remotes["mcp__"+out[i].Name+"__"]
		if !ok {
			continue
		}
		st, err := r.State()
		out[i].State = st.String()
		if err != nil {
			out[i].Failure = err.Error()
		}
	}
	return out
}

// UserDir is the user's directory the session reads its own sources
// from (Options.UserDir), ~/.dax, empty for none: a front's commands
// that change what the user keeps there, such as approving a skill the
// model wrote, act on it.
func (s *Session) UserDir() string { return s.opts.UserDir }

// SetThink turns reasoning on or off for later runs, fitted to the
// model in force, and keeps the setting for a later SetModel. The
// sub-agents follow it from their next call.
func (s *Session) SetThink(on bool) error {
	s.live.mu.Lock()
	s.live.think = on
	s.live.mu.Unlock()
	o := s.opts
	o.Think = on
	a := s.ag
	cfg := a.Config()
	cfg.Reasoning = o.reasoningFor(context.Background(), cfg.ModelName)
	return a.SetConfig(cfg)
}

// Compact has the next request fold the conversation whatever the
// compaction budget, with instructions, when not empty, for that fold
// alone (/compact): the fold a request over budget makes, noted and
// recorded as one. The next request is the main agent's next model
// call; a sub-agent's call in that run comes after it. With compaction
// off it returns agentkit.ErrNoCompaction.
func (s *Session) Compact(ctx context.Context, instructions string) error {
	// The kit's next request of any agent takes the fold, a sub-agent's
	// among them, so it is asked for only between runs.
	if s.ag.State().Running {
		return errors.New("compact waits for the run to end")
	}
	if err := s.ctl.Compact(ctx, instructions); err != nil {
		if errors.Is(err, agentkit.ErrNoCompaction) {
			return fmt.Errorf("compaction is off for this session (-compact 0, or the model's window is not known; give -compact N): %w", err)
		}
		return err
	}
	return nil
}

// Forward puts port on the workspace's loopback, a server something
// started where the tools act (a dev server in a box, say), on this
// machine's loopback alone, and returns its URL there, http://127.0.0.1:<p>
// (agentworkspace's Forward, RFC 0001 §9): each connection to it is a
// stream to the port, over whatever carries the workspace. It is the
// person's act, as running a command is; no tool opens one. A port
// forwarded already gives the URL it has; every forward ends with the
// session. A workspace that cannot connect to a port refuses, as
// unsupported.
func (s *Session) Forward(ctx context.Context, port int) (string, error) {
	s.fwdMu.Lock()
	defer s.fwdMu.Unlock()
	if ln, ok := s.forwards[port]; ok {
		return "http://" + ln.Addr().String(), nil
	}
	// The forward outlives the call that asked for it.
	ln, err := workspace.Forward(context.WithoutCancel(ctx), s.ws, port)
	if err != nil {
		return "", err
	}
	if s.forwards == nil {
		s.forwards = map[int]net.Listener{}
	}
	s.forwards[port] = ln
	return "http://" + ln.Addr().String(), nil
}

// Close detaches the recorder and releases the MCP server and the
// store. A second Close returns the first's error and does nothing: a
// front that moved to another session (Reopen) has closed this one.
func (s *Session) Close() error {
	s.closing.Do(func() { s.closeErr = s.close() })
	return s.closeErr
}

func (s *Session) close() error {
	if s.detach != nil {
		s.detach()
	}
	s.fwdMu.Lock()
	for port, ln := range s.forwards {
		ln.Close()
		delete(s.forwards, port)
	}
	s.fwdMu.Unlock()
	err := s.Kit.Close()
	if s.set != nil {
		s.set.ReleaseAll()
	}
	s.closeTools()
	s.closeHost()
	if s.ownWS {
		s.ws.Close()
	}
	if c, ok := s.store.(io.Closer); ok && s.own {
		if cerr := c.Close(); err == nil {
			err = cerr
		}
	}
	return err
}

// closeTools closes the executor of the extensions' tools, once.
func (s *Session) closeTools() {
	if s.x != nil {
		s.x.Close()
		s.x = nil
	}
}

// closeHost closes the workspace on this machine the session's
// secret-holding servers placed here started in (hostStarter), once.
func (s *Session) closeHost() {
	s.hostMu.Lock()
	defer s.hostMu.Unlock()
	if s.host != nil {
		s.host.Close()
		s.host = nil
	}
}

// readOnly opens the store for reading, without the session locks, so
// a session a running dax holds can be listed, verified and projected.
func readOnly(root string) (*cas.Store, error) { return cas.Open(root, cas.WithReadOnly()) }

// Verify rebuilds every model call recorded in a session and checks it
// against the stored request hash, then holds the path to the leaf to
// RFC 0001's records rules. It returns the number of responses checked
// and what failed.
func Verify(ctx context.Context, root, id string) (int, []error, error) {
	store, err := readOnly(root)
	if err != nil {
		return 0, nil, err
	}
	defer store.Close()
	sess, err := store.Open(ctx, id)
	if err != nil {
		return 0, nil, err
	}
	n := 0
	var failed []error
	for _, e := range sess.Entries() {
		if _, ok := e.(*agentsession.ResponseEntry); !ok {
			continue
		}
		n++
		if err := sess.Verify(e.Base().ID); err != nil {
			failed = append(failed, err)
		}
	}
	if err := sess.VerifyRecords(sess.Leaf()); err != nil {
		failed = append(failed, err)
	}
	return n, failed, nil
}

// UnhashedCause is a reason the recorder wrote for leaving requests
// unhashed, and how many responses after it went without a hash.
type UnhashedCause struct {
	Entry string
	session.Unhashed
	Responses int
}

// Unhashed lists the causes the recorder gave, in the session's order,
// for the responses it wrote with no request hash. A response with no
// hash before any cause was written by a recorder that gave none.
func Unhashed(ctx context.Context, root, id string) ([]UnhashedCause, error) {
	store, err := readOnly(root)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	sess, err := store.Open(ctx, id)
	if err != nil {
		return nil, err
	}
	var causes []UnhashedCause
	for _, e := range sess.Entries() {
		switch e := e.(type) {
		case *agentsession.CustomEntry:
			if e.NS != session.UnhashedNS {
				continue
			}
			c := UnhashedCause{Entry: e.ID}
			if err := json.Unmarshal(e.Data, &c.Unhashed); err != nil {
				return causes, fmt.Errorf("%s entry %s: %w", session.UnhashedNS, e.ID, err)
			}
			causes = append(causes, c)
		case *agentsession.ResponseEntry:
			if e.RequestHash == "" && len(causes) > 0 {
				causes[len(causes)-1].Responses++
			}
		}
	}
	return causes, nil
}

// Project writes a recorded session as an RFC 0001 JSONL file into dir,
// <id>.jsonl, for tools that read files rather than the store, such as
// agentsession show and verify. It returns the file's path.
func Project(ctx context.Context, root, id, dir string) (string, error) {
	store, err := readOnly(root)
	if err != nil {
		return "", err
	}
	defer store.Close()
	return store.ProjectDir(ctx, dir, id)
}

// Import reads an RFC 0001 JSONL session, one an earlier dax wrote,
// into the store as the session's record, so -list, -resume and -verify
// reach it. It returns the session's ID.
func Import(ctx context.Context, root, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	store, err := openStore(root)
	if err != nil {
		return "", err
	}
	defer store.Close()
	sess, err := store.Import(ctx, f, true)
	if err != nil {
		return "", err
	}
	return sess.Header().ID, nil
}

// GC packs the store's loose objects, or with sweep repacks everything
// into one pack and drops what no session needs, sparing objects
// younger than grace. It returns the number of objects packed or
// removed. A dax holding a session may keep writing meanwhile.
func GC(ctx context.Context, root string, sweep bool, grace time.Duration) (int, error) {
	store, err := openStore(root)
	if err != nil {
		return 0, err
	}
	defer store.Close()
	if sweep {
		return store.Sweep(ctx, grace)
	}
	return store.Pack(ctx)
}

// Repair rewrites a session whose log a crash or the disk damaged, so
// it opens again and the store can be swept: every record that still
// reads is kept, with the entries whose objects are whole, and the
// damaged log is kept beside it.
func Repair(ctx context.Context, root, id string) (cas.RepairReport, error) {
	store, err := openStore(root)
	if err != nil {
		return cas.RepairReport{}, err
	}
	defer store.Close()
	return store.Repair(ctx, id, cas.RepairOptions{})
}

// mcpDial makes the transport an MCP server is started over, each time
// it is started: its first start, and each restart after its session
// ended (mcpclient.WithReconnect).
type mcpDial func(context.Context) (mcp.Transport, error)

// offeredServers are the servers sp offers, by says who, as the session
// connects them: each started by name there, and listed (Info.MCP) as
// placed, at at. One name offered twice is refused, since it would be
// two servers under one prefix, and one's rules the other's; so is a name agentworkspace does not allow a
// server (workspace.ValidServerName: no underscore), which remote's
// client already refuses but a provider given in Options need not: "u_"
// would have tools mcp__u___<tool>, which box.confine takes for unit
// u's.
func offeredServers(ctx context.Context, sp workspace.ServerProvider, by string, placed Placement, at string) ([]MCPServer, error) {
	list, err := sp.Servers(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s's MCP servers: %w", by, err)
	}
	var out []MCPServer
	for _, u := range list {
		if !workspace.ValidServerName(u.Name) {
			return nil, fmt.Errorf("%s offers an MCP server called %q, which is not a server's name: 1 to 32 lowercase letters, digits and inner hyphens", by, u.Name)
		}
		if slices.ContainsFunc(out, func(m MCPServer) bool { return m.Name == u.Name }) {
			return nil, fmt.Errorf("%s offers two MCP servers called %s", by, u.Name)
		}
		out = append(out, MCPServer{Name: u.Name, Description: u.Description, offeredBy: sp, offeredAs: placed, offeredAt: at, offeredSecrets: u.Secrets})
	}
	return out, nil
}

// checkServersWorkspace is the servers workspace sw as the server
// provider it must be, beside ws, the session's: one that offers
// servers, that is not ws, and whose grant is servers alone
// (config.ServersAlone): no exec, with which the session runs commands
// as its servers' user and reads a running server's environment from
// /proc; no write, with which it changes a server's code or config to
// print its secret past the redaction; and no read, with which it reads
// what a server keeps in the directory, and /proc too when the host
// serves its root. That grant is the point of the seam: a secret holder
// is safe under it alone (agentworkspace's remote.ScopeServers). The
// grant is the scopes its client reports, as the server said them at
// the dial; a workspace that is not served reports none, and is
// refused.
func checkServersWorkspace(sw, ws workspace.Workspace) (workspace.ServerProvider, error) {
	d := sw.Descriptor()
	sp, offers := sw.(workspace.ServerProvider)
	if !offers {
		return nil, fmt.Errorf("the servers workspace %s offers no MCP servers: its grant lacks servers, or it is not a served workspace", where(d))
	}
	if wd := ws.Descriptor(); wd.Kind == d.Kind && wd.Ref == d.Ref && (d.Ref != "" || wd.Root == d.Root) {
		return nil, fmt.Errorf("the servers workspace %s is the session's workspace; it stands beside it, so name another or leave it out", where(d))
	}
	scopes, served := config.Scopes(sw)
	if !served {
		return nil, fmt.Errorf("the servers workspace %s is not a served workspace, whose grant would say it may start its servers alone; serve it with dax workspace -scopes servers", where(d))
	}
	if !config.ServersAlone(sw) {
		return nil, fmt.Errorf("the servers workspace %s grants more than servers (%s), with which the session could read or change what its servers hold, their secrets among it; serve it with dax workspace -scopes servers", where(d), scopeNames(scopes))
	}
	return sp, nil
}

// scopeNames is a grant's scopes, comma-separated and cleaned: a
// served workspace's server names them.
func scopeNames(scopes []remote.Scope) string {
	names := make([]string, len(scopes))
	for i, s := range scopes {
		names[i] = string(s)
	}
	return render.Clean(strings.Join(names, ","))
}

// mcpServer is the dial of an MCP server started from a command line,
// split as a shell splits words with nothing expanded (cmdline.Split),
// with its diagnostics on dax's stderr. In a workspace that can start a
// process (workspace.Starter) the server runs there, at its root, with
// the workspace's environment, over the process's pipes
// (mcpworkspace.CommandTransport); in one that cannot, it runs on this
// machine (mcpTransport), with env as its whole environment. An
// executor's view is a Starter, so with -executor every such server
// runs in the executor or not at all, and a restart starts it in the
// executor again, over the executor's connection of the moment. A
// server placed on the host (MCPServer.Host) is mcpTransport's alone.
func mcpServer(ws workspace.Workspace, command string, env []string) (mcpDial, error) {
	starter, ok := ws.(workspace.Starter)
	if !ok {
		return mcpTransport(command, env)
	}
	fields, err := splitCommand(command)
	if err != nil {
		return nil, err
	}
	// A server's diagnostics are text from a program the model may have
	// chosen; they get the same cleaning as its output. Each Connect
	// starts the server anew, so one transport serves every start.
	t := mcpworkspace.CommandTransport(starter, workspace.Command{Args: fields, Stream: render.CleanWriter(warnings{})})
	return func(context.Context) (mcp.Transport, error) { return t, nil }, nil
}

// mcpOffered is the dial of the server called name that the workspace
// offers (workspace.ServerProvider): a box host's unit, started where
// the box host runs it, its diagnostics cleaned onto dax's stderr.
func mcpOffered(sp workspace.ServerProvider, name string) mcpDial {
	t := mcpworkspace.ServerTransport(sp, name, render.CleanWriter(warnings{}))
	return func(context.Context) (mcp.Transport, error) { return t, nil }
}

// mcpTransport is the dial of an MCP server started on this machine
// from a command line, split as mcpServer splits it, with env as its
// whole environment and its diagnostics on dax's stderr. The kit's own
// command transport would hand the server dax's environment, keys
// included. A command transport connects once, so each dial builds a
// new one.
func mcpTransport(command string, env []string) (mcpDial, error) {
	fields, err := splitCommand(command)
	if err != nil {
		return nil, err
	}
	return func(context.Context) (mcp.Transport, error) {
		cmd := exec.Command(fields[0], fields[1:]...)
		cmd.Env = slices.Clone(env)
		// A server's diagnostics are text from a program the model may
		// have chosen; they get the same cleaning as its output.
		cmd.Stderr = render.CleanWriter(warnings{})
		return &mcp.CommandTransport{Command: cmd}, nil
	}, nil
}

// mcpDialOf is the dial of m wherever it runs, and m as Info.MCP lists
// it, placed by the same choice: one the workspace offers, by name
// there; at its URL; on this machine; or as mcpServer places a command,
// through an Offering of its own when it holds secrets (mcpHolding). A
// project's command server is placed in the workspace or not at all:
// in a workspace elsewhere that cannot start a process (a served one
// whose token lacks exec), it is refused rather than moved to this
// machine, as an executor that cannot start one fails it. Your own
// servers keep the rule that they run here then.
func (s *Session) mcpDialOf(m MCPServer) (mcpDial, MCPServerInfo, error) {
	info := MCPServerInfo{Name: m.Name, Description: m.Description, Secrets: len(m.SecretEnv) > 0, HoldsSecrets: m.offeredSecrets}
	placed := func(p Placement, at string, dial mcpDial, err error) (mcpDial, MCPServerInfo, error) {
		info.Placement, info.Where = p, at
		return dial, info, err
	}
	switch {
	case m.offeredBy != nil:
		return placed(m.offeredAs, m.offeredAt, mcpOffered(m.offeredBy, m.Name), nil)
	case m.URL != "":
		dial, err := mcpURL(m.URL, m.TokenEnv)
		return placed(PlacedURL, urlPlace(m.URL), dial, err)
	case len(m.SecretEnv) > 0:
		dial, err := s.mcpHolding(m)
		return placed(PlacedHost, "", dial, err)
	case m.Host:
		dial, err := mcpTransport(m.Command, s.env)
		return placed(PlacedHost, "", dial, err)
	}
	_, starts := s.ws.(workspace.Starter)
	if !starts && m.Project {
		if d := s.ws.Descriptor(); d.Kind != workspace.KindLocal {
			return nil, info, fmt.Errorf("the project's server would run on this machine, since its workspace, %s, cannot start a process; a project's servers run in its workspace or not at all: serve the workspace with a token that may run commands (dax workspace's does), or leave this server out of the project's config", where(d))
		}
	}
	// mcpServer starts it in the workspace when the workspace can start
	// a process, and on this machine otherwise.
	dial, err := mcpServer(s.ws, m.Command, s.env)
	if !starts {
		return placed(PlacedHost, "", dial, err)
	}
	return placed(PlacedWorkspace, where(s.ws.Descriptor()), dial, err)
}

// heldServer is the name a secret-holding server is offered under in
// the one-server Offering that starts it (mcpHolding): its tools take
// the configured name's prefix, never this.
const heldServer = "server"

// mcpHolding is the dial of a command's server that holds secrets
// (MCPServer.SecretEnv), which runs on this machine alone, placed on
// the host (checkSecretEnv): a secret stays on the machine whose
// environment holds it, and is never sent to a workspace, where the
// session's own bash would run beside the server as its user. It is
// started as a workspace's offered server is, by internal/offer, the
// path dax workspace -servers offers its servers through, in this
// machine's working directory (hostStarter): given the variables from
// this process's environment, which no other child gets, and read
// through the secrets filter, so what it writes reaches the session,
// the model and the record redacted of their values. A variable that
// is not set, or is too short to redact, fails the session before
// anything starts.
func (s *Session) mcpHolding(m MCPServer) (mcpDial, error) {
	for _, v := range m.SecretEnv {
		switch value := os.Getenv(v); {
		case value == "":
			return nil, fmt.Errorf("secret_env %s is not set; the server is not started without it", v)
		case len(value) < offer.MinSecretBytes:
			return nil, fmt.Errorf("secret_env %s is shorter than %d bytes, too short to redact without breaking the server's own messages", v, offer.MinSecretBytes)
		}
	}
	h, err := s.hostStarter()
	if err != nil {
		return nil, err
	}
	o, err := offer.Offer(h, []offer.Server{{Name: heldServer, Command: m.Command, SecretEnv: m.SecretEnv}}, os.LookupEnv)
	if err != nil {
		return nil, err
	}
	return mcpOffered(o, heldServer), nil
}

// hostStarter is this machine's working directory as a workspace whose
// processes start with the session's scrubbed environment, s.env: where
// a secret-holding server placed here starts, as mcpTransport starts
// any other.
func (s *Session) hostStarter() (*workspace.Local, error) {
	s.hostMu.Lock()
	defer s.hostMu.Unlock()
	if s.host != nil {
		return s.host, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	h, err := workspace.NewLocal(dir, s.env)
	if err != nil {
		return nil, fmt.Errorf("this machine's directory: %w", err)
	}
	s.host = h
	return h, nil
}

// checkSecretEnv refuses a server's SecretEnv that a project's config
// gave, that is for a URL, that is for a server not placed on this
// machine (a secret is never sent to a workspace: config.SecretInWorkspace),
// or that names another credential's variable (the model's key, a
// server's token, an extension's secret, a served workspace's or hub's
// token) or one PassEnv hands every child: each would give a secret to
// what should not have it.
func checkSecretEnv(o Options) error {
	extSecrets := extension.SecretsOf(o.Extensions)
	for _, m := range o.MCP {
		if len(m.SecretEnv) == 0 {
			continue
		}
		switch {
		case m.Project:
			return fmt.Errorf("mcp %s: secret_env: a project's server is given none of your secrets", m.Name)
		case m.URL != "":
			return fmt.Errorf("mcp %s: secret_env is for a command; a URL's token goes in token_env", m.Name)
		case !m.Host:
			return config.SecretInWorkspace(m.Name)
		}
		for _, v := range m.SecretEnv {
			why := ""
			switch {
			case v == "":
				why = "is not a variable name"
			case v == o.KeyEnv:
				why = "holds the model's key"
			case config.IsTokenVariable(v):
				why = "is a served workspace's token"
			case slices.Contains(o.PassEnv, v):
				why = "is in PassEnv, which hands it to every child"
			case slices.ContainsFunc(o.MCP, func(t MCPServer) bool { return t.TokenEnv == v }):
				why = "is a server's token_env"
			case slices.Contains(extSecrets, v):
				why = "is an extension's secret"
			}
			if why != "" {
				return fmt.Errorf("mcp %s: secret_env %s %s; name a variable of the server's own", m.Name, v, why)
			}
		}
	}
	return nil
}

// mcpStart is what the session connects the MCP server m to, whether
// the config or /mcp add named it: the transport of its first start,
// and the client's options, which name its tools mcp__<name>__<tool>
// and start it again when its session ends (a process that exited, or,
// with -executor, the connection it was started over lost), each
// restart reported (mcpRestarted); and m as Info.MCP lists it.
func (s *Session) mcpStart(ctx context.Context, m MCPServer) (mcp.Transport, []mcpclient.Option, MCPServerInfo, error) {
	dial, info, err := s.mcpDialOf(m)
	if err != nil {
		return nil, nil, info, err
	}
	t, opts, err := s.mcpConnect(ctx, m.Name, dial)
	return t, opts, info, err
}

// mcpConnect is mcpStart's for a dial already made.
func (s *Session) mcpConnect(ctx context.Context, name string, dial mcpDial) (mcp.Transport, []mcpclient.Option, error) {
	prefix, err := mcpPrefix(name)
	if err != nil {
		return nil, nil, err
	}
	t, err := dial(ctx)
	if err != nil {
		return nil, nil, err
	}
	return t, []mcpclient.Option{
		mcpclient.WithPrefix(prefix),
		mcpclient.WithReconnect(dial),
		mcpclient.WithReconnected(s.mcpRestarted(name)),
		mcpclient.WithEnded(s.mcpEnded(name)),
	}, nil
}

// mcpEnded is the hook of the server called name for the end of each of
// its sessions (mcpclient.WithEnded), seen when it ends, not when a
// call next finds it so: one of dax's warnings, so the user knows the
// server is down, and whatever it held gone, before any call. Info
// lists it ended until the next call to one of its tools starts it
// again, which the restart's warning and record entry then say.
func (s *Session) mcpEnded(name string) func(error) {
	return func(err error) {
		fmt.Fprintf(warnings{}, "dax: MCP server %s's session ended (%v); the next call to one of its tools starts it again\n", name, err)
	}
}

// mcpRestartNS is the namespace of the entry the session's record gets
// for each attempt to start one of its MCP servers again, an
// mcpRestart: a restart says the server lost whatever it held (open
// handles, subscriptions), which no tool call shows when its tools are
// the same.
const mcpRestartNS = "dax.mcp.restart"

// mcpRestart is one attempt to start an MCP server again after its
// session ended, as the session's record has it under mcpRestartNS.
type mcpRestart struct {
	// Server is the server's name, the config's or /mcp add's.
	Server string `json:"server"`
	// Why is what ended the session the restart replaces.
	Why string `json:"why"`
	// OK says the server started again. Error is why it did not; the
	// next call to one of its tools tries again.
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Implementation and Version are what the restarted server calls
	// itself.
	Implementation string `json:"implementation,omitempty"`
	Version        string `json:"version,omitempty"`
	// Changed says its tools differ from the ones before the restart;
	// Added and Removed name the tools that came and went. Changed
	// with neither is a tool whose definition or claims changed.
	Changed bool     `json:"changed,omitempty"`
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
}

// mcpSessionEnded is an mcpRestart's Why: the client does not say what
// ended the session, a process that exited or a connection lost.
const mcpSessionEnded = "the server's session ended"

// mcpRestarted is the hook of the server called name for each attempt
// to start it again (mcpclient.WithReconnected): one of dax's warnings,
// and an mcpRestart in the session's record. The attempt runs on the
// client's own, outside any run, so the entry is written at the
// session's leaf, beside the calls, never as one.
func (s *Session) mcpRestarted(name string) func(mcpclient.ReconnectInfo) {
	return func(info mcpclient.ReconnectInfo) {
		r := mcpRestartOf(name, info)
		fmt.Fprintf(warnings{}, "dax: %s\n", r.warning())
		if s.Kit == nil {
			return
		}
		if rec := s.Kit.Recorder(); rec != nil {
			// A write that fails is lost, as a verdict's is.
			_, _ = rec.Annotate(context.Background(), mcpRestartNS, r)
		}
	}
}

// mcpRestartOf is an attempt to start the server called name again as
// the record has it.
func mcpRestartOf(name string, info mcpclient.ReconnectInfo) mcpRestart {
	r := mcpRestart{Server: name, Why: mcpSessionEnded}
	if info.Err != nil {
		r.Error = info.Err.Error()
		return r
	}
	r.OK = true
	r.Implementation, r.Version = info.Server.Name, info.Server.Version
	r.Changed = info.Changed
	for _, n := range info.After {
		if !slices.Contains(info.Before, n) {
			r.Added = append(r.Added, n)
		}
	}
	for _, n := range info.Before {
		if !slices.Contains(info.After, n) {
			r.Removed = append(r.Removed, n)
		}
	}
	return r
}

// warning is the line dax warns with about r.
func (r mcpRestart) warning() string {
	if !r.OK {
		return fmt.Sprintf("MCP server %s: %s", r.Server, r.Error)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "MCP server %s ended and was started again", r.Server)
	if r.Implementation != "" {
		fmt.Fprintf(&b, " (%s %s)", r.Implementation, r.Version)
	}
	b.WriteString("; whatever it held is gone")
	switch {
	case !r.Changed:
		b.WriteString(", and its tools are the same")
	case len(r.Added) == 0 && len(r.Removed) == 0:
		b.WriteString(", and a tool's definition changed")
	default:
		b.WriteString(", and its tools changed:")
		if len(r.Added) > 0 {
			b.WriteString(" added " + strings.Join(r.Added, ", "))
			if len(r.Removed) > 0 {
				b.WriteString(";")
			}
		}
		if len(r.Removed) > 0 {
			b.WriteString(" removed " + strings.Join(r.Removed, ", "))
		}
	}
	return b.String()
}

// splitCommand is an MCP server's command line as its program and
// arguments.
func splitCommand(command string) ([]string, error) {
	fields, err := cmdline.Split(command)
	if err != nil {
		return nil, fmt.Errorf("command %q: %w", command, err)
	}
	if len(fields) == 0 || strings.TrimSpace(fields[0]) == "" {
		return nil, errors.New("empty command")
	}
	return fields, nil
}

// childPolicy decides a call of a sub-agent called name under the
// parent's policy and the extensions' BeforeToolCall hooks (hook, nil
// for none), folded deny over ask over allow: a block blocks it, an
// allow lets it run (with the arguments a hook rewrote, a bash line
// dax-coding stamped say), and a call either asks about is put to the
// user, and runs rewritten too if they approve. With no one to ask, it
// is refused. A policy that is off governs nothing, the child included.
//
// The facts of the sub-agent's model response are held in set for the
// decisions about its calls (executor.Set.PinBatch), so a call's
// verdict, the hooks' rewrite and the decision about that rewrite come
// from one reading of the response. The batch is let go before a
// question, so a person's slow answer holds no reading open; the next
// call of the response is read in a batch of its own.
func (s *Session) childPolicy(name string, eng *atomic.Pointer[agentpolicy.Engine], hook func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error), set *executor.Set) func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
	return func(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
		e := eng.Load()
		if e == nil {
			return nil, nil
		}
		// A sub-agent's call is decided without box.confine's own claim
		// (confine.go): only what the policy allows anyway runs unasked.
		ctx = childDecision(ctx, name)
		bctx, done := set.PinBatch(ctx, info)
		defer done() // a no-op after the done below, which comes before the question
		v, err := e.Would(bctx, info)
		if err != nil {
			return &agentturn.ToolDecision{Action: agentturn.Block, Reason: "the policy could not decide this call: " + err.Error()}, nil
		}
		v = s.sessionGrantFor(bctx, e, info, v)
		s.recordVerdict(ctx, v)
		var h *agentturn.ToolDecision
		if hook != nil {
			h, err = hook(bctx, info)
		}
		done()
		if err != nil {
			return &agentturn.ToolDecision{Action: agentturn.Block, Reason: "a hook could not decide this call: " + err.Error()}, nil
		}
		switch {
		case v.Action == agentturn.Block:
			return &agentturn.ToolDecision{Action: agentturn.Block, Reason: v.Reason, By: agentpolicy.ByPolicy}, nil
		case h != nil && h.Action == agentturn.Block:
			return h, nil
		case v.Action == agentturn.Defer || h != nil && h.Action == agentturn.Defer:
			why, subject := v.Reason, v.Subject
			if v.Action != agentturn.Defer {
				why, subject = h.Reason, ""
			}
			var args json.RawMessage
			if h != nil {
				args = h.Args
			}
			set.Release(bctx)
			// The deferral stands, and tools/agent's WithAskParent puts
			// it to the task call's elicitor with agentturn.Ask: the
			// question the fronts show is this reason (elicit), and
			// with nobody to ask the sub-agent ends input-required.
			// A decline is told in dax's words, as a person's refusal
			// of the main agent's call is (Refusal), and what the
			// answerer said follows them (agentturn.Ask, #304).
			return &agentturn.ToolDecision{Action: agentturn.Defer, Reason: childAsks(name, why), Subject: subject, Args: args, By: agentpolicy.ByPolicy,
				DeclineText: childDeclined(why)}, nil
		}
		// The verdict is returned rather than left implicit, so the
		// sub-agent's session records a decision for each call, as the
		// main agent's does through the engine's observer.
		d := &agentturn.ToolDecision{Action: agentturn.Allow, Reason: v.Reason, By: agentpolicy.ByPolicy}
		if h != nil && h.Args != nil {
			d.Args = h.Args
		}
		return d, nil
	}
}

// sessionGrantFor is v, a sub-agent's verdict under its own grant
// scope, or the verdict under its session's scope when a grant the
// person made for the session ("a", policy.SourceSession) allows the
// call there: the person's grant reaches the session's sub-agents,
// where a skill its parent read does not. It applies only where v
// asks for want of a rule, or is dontAsk's refusal of such a call, and
// never to an A2A caller's sub-agent, whose conversation is not the
// session's.
func (s *Session) sessionGrantFor(ctx context.Context, e *agentpolicy.Engine, info agentturn.ToolCallInfo, v agentpolicy.Verdict) agentpolicy.Verdict {
	if _, peer := ctx.Value(peerKey{}).(string); peer || s.Kit == nil {
		return v
	}
	asks := v.Action == agentturn.Defer && v.Rule == nil
	refused := v.Action == agentturn.Block && v.Reason == policy.DontAskNote
	if !asks && !refused {
		return v
	}
	s.grants.mu.Lock()
	none := len(s.grants.session) == 0
	s.grants.mu.Unlock()
	if none {
		return v
	}
	g, err := e.Would(agentpolicy.ContextWithGrantScope(ctx, s.Kit.GrantScope(context.Background())), info)
	if err != nil || g.Action != agentturn.Allow || g.Rule == nil || g.Rule.Source.Name != policy.SourceSession {
		return v
	}
	return g
}

// recordVerdict writes a sub-agent's verdict to its run's session under
// agentpolicy.VerdictNS, as the kit does for each of the main agent's
// engine verdicts: the rule, the reason and the part of the call it was
// about (its Subject) are on the record beside the call's decision,
// which keeps the policy's reason as it gave it. The engine's Would
// observes nothing, so the hook writes it. A write that fails is lost,
// as an observer's is.
func (s *Session) recordVerdict(ctx context.Context, v agentpolicy.Verdict) {
	rec := agentkit.RecorderFromContext(ctx)
	if rec == nil {
		rec = s.Kit.Recorder()
	}
	if rec == nil {
		return
	}
	ns, data := v.Record()
	_, _ = rec.Annotate(ctx, ns, json.RawMessage(data))
}

// childAsks is the question a sub-agent's deferred call puts, as the
// fronts show it and the record keeps it: the sub-agent's name, then
// the policy's reason.
func childAsks(name, why string) string { return "the " + name + " sub-agent asks: " + why }

// childDeclined is what a sub-agent's model reads for a call of its own
// declined when asked, the policy's reason why it asked in brackets:
// policy.NoWorkAround's rule, but with no user of its own to let
// decide, it says so in the result its caller reads. agentturn adds
// what the answerer said after it.
func childDeclined(why string) string {
	rule, _, _ := strings.Cut(policy.NoWorkAround, ";")
	return "declined when asked about this call (" + why + "). " + rule + "; if it is still needed, say so in your result"
}

// isChildAsk reports whether reason is a sub-agent's question
// (childAsks).
func isChildAsk(reason string) bool {
	rest, ok := strings.CutPrefix(reason, "the ")
	return ok && strings.Contains(rest, " sub-agent asks: ")
}

// secrets are the variables taken out of the environment the session's
// tools and MCP servers get whatever their names (Secrets).
func (o Options) secrets() []string { return Secrets(o.Extensions, o.KeyEnv, o.MCP) }

// Secrets are the variables every child of a session loses whatever
// their names: the extensions' Secrets, the model's key's variable
// keyEnv, and each MCP server's TokenEnv and SecretEnv. A program that builds the
// session's workspace, or starts its executor, itself scrubs those
// environments of these, as dax's command line does, since
// Options.Workspace carries its own environment.
func Secrets(exts []extension.Extension, keyEnv string, servers []MCPServer) []string {
	out := extension.SecretsOf(exts)
	if keyEnv != "" {
		out = append(out, keyEnv)
	}
	for _, m := range servers {
		if m.TokenEnv != "" {
			out = append(out, m.TokenEnv)
		}
		// A server's secrets are its own (MCPServer.SecretEnv).
		out = append(out, m.SecretEnv...)
	}
	return out
}
