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
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	"github.com/ChristopherDavenport/agentsmd"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agenttool/mcpclient"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/compact"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/internal/config"
	"github.com/ChristopherDavenport/dax/internal/render"
	"github.com/ChristopherDavenport/dax/policy"
	"github.com/ChristopherDavenport/dax/tool"
	"github.com/ChristopherDavenport/dax/workspace"
)

// Version is what the session header names as the harness version.
const Version = "0.1.0-dev"

// What the model sees for a call that will never get a real answer.
// Each says what the loop's PendingReason says and no more: a call cut
// off in flight may have taken effect, a deferred call never ran, and
// a call found unanswered in a transcript the record says nothing
// about is one nobody can account for.
const (
	abortedOutput  = "Error: this call was cut off while it was running; it may have taken effect."
	deniedOutput   = "Error: the user denied this call."
	unknownOutput  = "Error: an earlier run left this call unanswered; whether it ran is not recorded."
	neverRanOutput = "Error: this call was cut off before it reached its tool; it did not run."
	heldOutput     = "Error: this call was waiting for approval when the session stopped, and may have run before it was held; it will not run again."
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
	// Model is the model name the requests carry.
	Model string
	// Fit is the reasoning effort to ask a model for in place of the
	// one Think implies, from what its vendor says it takes; nil asks
	// as configured. It is applied where each configuration is built,
	// never to a request on its way out, so the session records the
	// request that was sent.
	Fit   func(ctx context.Context, model string, want openresponses.ReasoningEffort) openresponses.ReasoningEffort
	Think bool
	// Effort is the reasoning effort Think asks for; empty is low.
	Effort openresponses.ReasoningEffort
	// Dir is the directory on this machine the session's instructions
	// are read from: the AGENTS.md chain and an extension's project
	// files (.dax/skills). With no Workspace it is also where the tools
	// act, as a workspace.Local.
	Dir string
	// Workspace is where the tools act and what the session records as
	// its cwd and workspace: a container, a remote runtime, or this
	// machine. Nil is a workspace.Local over Dir, with an environment
	// scrubbed of credentials (PassEnv, KeyEnv), which the session
	// closes; one given here is the caller's to close.
	Workspace workspace.Workspace
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

	// AgentsMD reads UserDir/AGENTS.md and the AGENTS.md chain from the
	// file system root down to Dir into the instructions.
	AgentsMD bool
	// Compact, when positive, is the estimated token budget above which
	// the transcript is folded before a call.
	Compact int
	// CompactServer folds through the server's compaction endpoint
	// (compact.New) instead of a local summary (compact.NewLocal).
	CompactServer bool
	// Policy is the user's and the project's rules; the session adds
	// the extensions' (Settings.Shipped) and builds the policy that
	// decides which calls run, ask or are refused. nil lets every call
	// run, and the extensions' BeforeToolCall hooks are not run.
	Policy *policy.Settings
	// Approve decides a call the policy asked about; reason is the
	// policy's. nil denies every call.
	Approve func(call *openresponses.FunctionCall, reason string) bool
	// Ask puts a sub-agent's call the policy asks about to the user from
	// inside the running call, and is preferred to Approve there: allow,
	// and the note the user typed with a refusal. Its context is the
	// call's, so an abort gives the question up. An error means nobody
	// could be asked. Nil falls back to Approve.
	Ask func(ctx context.Context, call *openresponses.FunctionCall, reason string) (allow bool, note string, err error)
	// Elicit answers a question a tool asks mid-call: an MCP server's
	// elicitation, or a nested call the policy asked about. nil leaves
	// every such question unasked, which the tool takes as a cancel.
	Elicit agenttool.Elicitor
	// MCP are stdio MCP servers started with the session, each offering
	// its tools as mcp__<Name>__<tool>. Their stderr is dax's.
	MCP []MCPServer
	// MaxReadBytes is the most bytes of a file a tool should read,
	// which extension.ToolEnv carries; zero is tool.DefaultMaxRead.
	MaxReadBytes int64
	// PassEnv names credential-looking variables MCP servers, and the
	// processes of the workspace.Local the session opens when Workspace
	// is nil, may still inherit; every other credential is removed from
	// their environment. A workspace given in Workspace carries its own
	// (Workspace.Env).
	PassEnv []string
	// KeyEnv is the variable the provider's key was read from. It is
	// removed from the same environments even when its name does not
	// look like a credential's, unless PassEnv names it.
	KeyEnv string
	// NoAgent builds the kit and no agent over it: a front that drives
	// the kit through its own backend (the terminal client) builds the
	// agent itself, and two agents on one recorder would both write
	// the session. Session.Agent is nil, and Prompt, Steer and the
	// other methods that drive it must not be called.
	NoAgent bool
	// Log receives dax's own notes: compactions, denials, skill grants.
	// nil discards them.
	Log func(format string, args ...any)
}

// MCPServer is a stdio MCP server to start.
type MCPServer struct {
	// Name is the prefix of the server's tools.
	Name string
	// Command is the command line.
	Command string
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
// agent running it and, when recording, the store it is written to.
type Session struct {
	Agent *agentturn.Agent
	Kit   *agentkit.Kit

	opts    Options
	live    live
	env     []string            // what bash and MCP servers start with
	refused []agentkit.Omission // repository files screened out before the kit
	ws      workspace.Workspace
	ownWS   bool // the session opened ws, and closes it
	files   *tool.Files
	tools   []agenttool.Tool // the extensions', which the session closes
	store   agentsession.Store
	own     bool // the session opened store, and closes it
	detach  func()
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

// TUIConfig is the adjustment a terminal client makes to the kit's
// agent configuration (kitbackend.WithConfig): a call the policy defers
// has what the policy was asking about added to its reason, since the
// reason is what the permission panel shows. The kit's engine says it
// in the verdict's subject: the part of a command line, the file a
// link leads to, the git config key.
func (s *Session) TUIConfig(cfg agentturn.Config) agentturn.Config {
	inner := cfg.BeforeToolCall
	eng := s.Kit.Engine()
	if inner == nil || eng == nil {
		return cfg
	}
	cfg.BeforeToolCall = func(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
		d, err := inner(ctx, info)
		if err != nil || d == nil || d.Action != agentturn.Defer || info.Call == nil {
			return d, err
		}
		if v, ok := eng.Deferred(info.RunID, info.Call.CallID); ok && v.Subject != "" && !strings.Contains(d.Reason, v.Subject) {
			d.Reason += "; about: " + v.Subject
		}
		return d, nil
	}
	return cfg
}

// Pending lists the calls awaiting outputs, in transcript order. The
// next Prompt answers them ahead of the user's message. A resumed
// session's calls carry what its record says of them, since the kit
// seeds the agent with the pending calls at the leaf.
func (s *Session) Pending() []agentturn.PendingCall { return s.Agent.State().Pending }

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
	ws, ownWS := o.Workspace, false
	if ws == nil {
		local, err := workspace.NewLocal(o.Dir, tool.DefaultEnv(o.PassEnv, o.KeyEnv))
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
	s := &Session{opts: o, store: store, own: own, ws: ws, ownWS: ownWS, files: tool.NewFiles(ws)}
	s.live.think, s.live.main = o.Think, o.Model
	ok := false
	defer func() {
		if !ok {
			s.closeTools()
			if ownWS {
				ws.Close()
			}
		}
	}()
	model := o.Streamer
	var engine atomic.Pointer[agentpolicy.Engine]
	// MCP servers run on this machine, whatever the workspace, and start
	// with this machine's environment scrubbed.
	env := tool.DefaultEnv(o.PassEnv, o.KeyEnv)
	s.env = env
	te := extension.ToolEnv{Workspace: ws, Files: s.files, MaxReadBytes: o.MaxReadBytes}
	a, err := build(o.Extensions, te)
	if err != nil {
		return nil, err
	}
	s.tools = a.tools

	// agentsText is the AGENTS.md part as the kit renders it, for a
	// sub-agent whose configuration is fixed before the kit is built.
	// The kit has no instruction budget, under which it renders the
	// chain whole, so the same options give the same text.
	var kopts []agentkit.Option
	agentsText := ""
	if o.AgentsMD {
		// The walk is done here, so that a file that is a link out of
		// the workspace can be left out; agentsmd is given the screened
		// files and a name that matches nothing, so it walks to no more.
		files, refused := agentsFiles(o.Dir)
		s.refused = append(s.refused, refused...)
		mdOpts := agentsmd.Options{
			Names:  []string{".dax-no-such-file"},
			Extra:  append([]string{filepath.Join(o.UserDir, "AGENTS.md")}, files...),
			Budget: 32 << 10,
		}
		kopts = append(kopts, agentkit.WithAgentsMD(o.Dir, mdOpts))
		res, err := agentsmd.Chain(o.Dir, mdOpts)
		if err != nil {
			return nil, fmt.Errorf("AGENTS.md: %w", err)
		}
		agentsText = agentsmd.Render(res.Files)
	}
	kopts = append(kopts, agentkit.WithTools(a.tools...))
	xenv := &sessionEnv{s: s, a: a, te: te, agentsMD: agentsText, eng: &engine}
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
	if o.Policy != nil {
		settings := *o.Policy
		settings.Shipped = append(slices.Clone(settings.Shipped), a.shipped...)
		p, err := policy.Build(settings)
		if err != nil {
			return nil, err
		}
		kopts = append(kopts, agentkit.WithPolicy(p, a.matchers, agentpolicy.WithAliases(a.aliases)))
		if h := a.hook(); h != nil {
			kopts = append(kopts, agentkit.WithBeforeToolCall(h))
		}
	}
	if o.Elicit != nil {
		kopts = append(kopts, agentkit.WithToolElicitor(agentpolicy.ByHuman, o.Elicit))
	}
	// Set whether or not a server is configured, since /mcp may add one.
	kopts = append(kopts, agentkit.WithMCPStderr(os.Stderr))
	for _, m := range o.MCP {
		prefix, err := mcpPrefix(m.Name)
		if err != nil {
			return nil, err
		}
		t, err := mcpTransport(m.Command, env)
		if err != nil {
			return nil, fmt.Errorf("mcp %s: %w", m.Name, err)
		}
		kopts = append(kopts, agentkit.WithMCPTransport(t, mcpclient.WithPrefix(prefix)))
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
			case f.Summary != nil:
				o.log("[compacted: %d item(s) folded, %d tokens before]", f.Split, f.TokensBefore)
			}
		})
		if o.CompactServer {
			c, ok := model.(compact.Compactor)
			if !ok {
				return nil, errors.New("-compact-server: this provider has no compaction endpoint")
			}
			kopts = append(kopts, agentkit.WithCompactor(c, o.Compact), fold)
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
			kopts = append(kopts, agentkit.WithCompaction(o.Compact, none), fold)
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
	if !o.NoAgent {
		s.Agent = agentturn.New(kit.Config(), kit.AgentOptions()...)
		s.detach = kit.Attach(s.Agent)
	}
	ok = true
	return s, nil
}

// header names the harness and the cwd: the workspace's root, the
// path the tools act in, on whichever machine that is.
func (s *Session) header() agentsession.Header {
	return agentsession.Header{
		CWD:     s.ws.Root(),
		Harness: &agentsession.Harness{Name: s.opts.Name, Version: s.opts.Version},
	}
}

// envOption is the environment entry the recorder asks for once per
// run: the directory the tools are rooted at and which file system it
// is in, from the workspace's descriptor (this machine, a container, a
// remote runtime). The recorder writes it only when it differs from the
// last one on the path, so a session that stays in one workspace holds
// one entry and a resume in another says so.
func (s *Session) envOption() session.Option {
	return session.WithEnv(func(context.Context) (*agentsession.EnvEntry, error) {
		d := s.ws.Descriptor()
		e := agentsession.NewEnvEntry(d.Root)
		e.SetWorkspace(d.Kind, d.Ref)
		return e, nil
	})
}

// New starts a fresh session.
func New(ctx context.Context, o Options) (*Session, error) {
	store, own, err := o.openStore()
	if err != nil {
		return nil, err
	}
	s, err := open(ctx, o, store, own, "")
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
	if err != nil {
		if c, ok := store.(io.Closer); ok && own {
			c.Close()
		}
		return nil, err
	}
	sess := s.Kit.Session()
	if t := sess.Truncated(); t != nil {
		fmt.Fprintf(stderr, "dax: session %s: %v (dropped)\n", id, t)
	}
	if f := sess.DeclaredFormat(); f != agentsession.Format {
		// The recorder raises the header before its first append, after
		// which no reader older than agentsession v0.0.18 opens it.
		fmt.Fprintf(stderr, "dax: session %s was written as %s; this dax writes %s, and readers before agentsession v0.0.18 refuse it from the next entry\n", id, f, agentsession.Format)
	}
	if cwd, now := sess.Header().CWD, s.ws.Root(); cwd != "" && cwd != now {
		fmt.Fprintf(stderr, "dax: session was recorded in %s, continuing in %s\n", cwd, now)
	}
	return s, nil
}

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

// Omitted is what the instruction layers considered and left out.
func (s *Session) Omitted() []agentkit.Omission {
	return append(append([]agentkit.Omission(nil), s.refused...), s.Kit.Omitted()...)
}

// Tools lists the tools the kit assembled, each with its source.
func (s *Session) Tools() []agentkit.ToolOrigin { return s.Kit.Tools() }

// Prompt sends one user message and runs until the agent is idle. Calls
// an abort left pending are answered ahead of the message, in the same
// model call; a call a resumed session left held for approval is put to
// Approve first. Calls the policy asked about are put to Approve, the
// calls it held beside them are released by the engine, and the run is
// resumed until it ends for another reason.
func (s *Session) Prompt(ctx context.Context, text string) (*agentturn.RunEnd, error) {
	// Memory written during the run names this session, and so does
	// each model call, for a server that groups calls by session; the
	// kit cannot put the ID on a context that is the host's.
	ctx = agentmemory.WithSession(ctx, s.ID())
	if id := s.ID(); id != "" {
		ctx = session.ContextWithSessionID(ctx, id)
	}
	var end *agentturn.RunEnd
	var err error
	if pending := s.Pending(); s.opts.Approve != nil && slices.ContainsFunc(pending, held) {
		// A call held for approval when the last process stopped never
		// ran and was never answered, so the user is asked about it
		// again, and the message follows its answer in the same run.
		s.Agent.Steer(openresponses.UserText(text))
		end, err = s.Agent.Resume(ctx, s.reanswer(pending)...)
	} else {
		var items openresponses.Items
		for _, p := range pending {
			items = append(items, openresponses.NewFunctionCallOutput(p.Call.CallID, output(p)))
		}
		items = append(items, openresponses.UserText(text))
		end, err = s.Agent.Prompt(ctx, items...)
	}
	for err == nil && end.Reason == agentturn.ReasonInputRequired {
		var answers []agentturn.Answer
		if answers, err = s.answer(ctx, end); err != nil {
			break
		}
		end, err = s.Agent.Resume(ctx, answers...)
	}
	return end, err
}

// held reports a call held for approval before its dispatch: it never
// ran, and a person may still allow it.
func held(p agentturn.PendingCall) bool {
	return p.Reason == agentturn.PendingDeferred && !p.Dispatched
}

// reanswer answers the calls a resumed session left pending: each held
// one as the user decides now, the rest with what the model is told of
// them.
func (s *Session) reanswer(pending []agentturn.PendingCall) []agentturn.Answer {
	answers := make([]agentturn.Answer, 0, len(pending))
	for _, p := range pending {
		switch {
		case !held(p):
			answers = append(answers, agentturn.Output(openresponses.NewFunctionCallOutput(p.Call.CallID, output(p))))
		case s.opts.Approve(p.Call, "held for approval when the session stopped"):
			answers = append(answers, agentturn.Approve(p.Call.CallID).WithBy(agentpolicy.ByHuman))
		default:
			s.opts.log("  ✗ %s denied", p.Call.Name)
			answers = append(answers, agentturn.Output(openresponses.NewFunctionCallOutput(p.Call.CallID, deniedOutput)).WithBy(agentpolicy.ByHuman))
		}
	}
	return answers
}

// answer asks the user about each call the policy asked about and lets
// the engine release the ones it only held. Every answer the user gave
// is recorded as a person's.
func (s *Session) answer(ctx context.Context, end *agentturn.RunEnd) ([]agentturn.Answer, error) {
	eng := s.Kit.Engine()
	answers := make([]agentturn.Answer, 0, len(end.Pending))
	for _, p := range end.Pending {
		reason := ""
		if eng != nil {
			v, ok := eng.Deferred(end.RunID, p.Call.CallID)
			if ok && v.Held {
				continue
			}
			reason = v.Reason
			if v.Subject != "" && !strings.Contains(reason, v.Subject) {
				// A compound command's question names the part it is
				// about, which is also where the policy says why.
				reason += "; about: " + v.Subject
			}
		}
		if s.opts.Approve != nil && s.opts.Approve(p.Call, reason) {
			answers = append(answers, agentturn.Approve(p.Call.CallID).WithBy(agentpolicy.ByHuman))
			continue
		}
		s.opts.log("  ✗ %s denied", p.Call.Name)
		answers = append(answers, agentturn.Output(openresponses.NewFunctionCallOutput(p.Call.CallID, deniedOutput)).WithBy(agentpolicy.ByHuman))
	}
	if eng == nil {
		return answers, nil
	}
	return eng.Release(ctx, end, answers...)
}

// Steer queues a user message for the next model call of the run in
// flight; idle, it is queued for the next run.
func (s *Session) Steer(text string) { s.Agent.Steer(openresponses.UserText(text)) }

// FollowUp queues a user message the run picks up when it would
// otherwise end.
func (s *Session) FollowUp(text string) { s.Agent.FollowUp(openresponses.UserText(text)) }

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
	cfg := s.Agent.Config()
	cfg.ModelName = name
	cfg.Reasoning = o.reasoningFor(context.Background(), name)
	if err := s.Agent.SetConfig(cfg); err != nil {
		return err
	}
	s.live.mu.Lock()
	s.live.main = name
	s.live.mu.Unlock()
	return nil
}

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
	prefix, err := mcpPrefix(name)
	if err != nil {
		return "", err
	}
	t, err := mcpTransport(command, s.env)
	if err != nil {
		return "", err
	}
	return s.Kit.AddMCPTransport(ctx, t, mcpclient.WithPrefix(prefix))
}

// RemoveMCP closes a server AddMCP started; its tools are not offered
// from the next run.
func (s *Session) RemoveMCP(label string) error { return s.Kit.RemoveMCP(label) }

// SetThink turns reasoning on or off for later runs, fitted to the
// model in force, and keeps the setting for a later SetModel. The
// sub-agents follow it from their next call.
func (s *Session) SetThink(on bool) error {
	s.live.mu.Lock()
	s.live.think = on
	s.live.mu.Unlock()
	o := s.opts
	o.Think = on
	cfg := s.Agent.Config()
	cfg.Reasoning = o.reasoningFor(context.Background(), cfg.ModelName)
	return s.Agent.SetConfig(cfg)
}

// Close detaches the recorder and releases the MCP server and the
// store.
func (s *Session) Close() error {
	if s.detach != nil {
		s.detach()
	}
	err := s.Kit.Close()
	s.closeTools()
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

// closeTools closes the extensions' tools that hold something, once.
func (s *Session) closeTools() {
	agenttool.Set(s.tools).Close()
	s.tools = nil
}

// readOnly opens the store for reading, without the session locks, so
// a session a running dax holds can be listed, verified and projected.
func readOnly(root string) (*cas.Store, error) { return cas.Open(root, cas.WithReadOnly()) }

// List returns the recorded sessions under root, newest first, for cwd
// when it is not empty.
func List(ctx context.Context, root, cwd string) ([]agentsession.Summary, error) {
	if !cas.IsStore(root) {
		return nil, nil
	}
	store, err := readOnly(root)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	var out []agentsession.Summary
	for sum, err := range store.List(ctx, agentsession.ListFilter{CWD: cwd}) {
		if err != nil {
			return out, err
		}
		out = append(out, sum)
	}
	return out, nil
}

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

// mcpTransport starts an MCP server from a command line, split on
// spaces, with env as its whole environment and its diagnostics on
// dax's stderr. The kit's own command transport would hand the server
// dax's environment, keys included.
func mcpTransport(command string, env []string) (mcp.Transport, error) {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return nil, errors.New("empty command")
	}
	cmd := exec.Command(fields[0], fields[1:]...)
	cmd.Env = env
	// A server's diagnostics are text from a program the model may have
	// chosen; they get the same cleaning as its output.
	cmd.Stderr = render.CleanWriter(stderr)
	return &mcp.CommandTransport{Command: cmd}, nil
}

// childPolicy decides a call of a sub-agent called name under the
// parent's policy and the extensions' BeforeToolCall hooks (hook, nil
// for none), folded deny over ask over allow: a block blocks it, an
// allow lets it run (with the arguments a hook rewrote, a bash line
// dax-coding stamped say), and a call either asks about is put to the
// user. With no one to ask, it is refused. A policy that is off governs
// nothing, the child included.
func (o Options) childPolicy(name string, eng *atomic.Pointer[agentpolicy.Engine], hook func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error)) func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
	return func(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
		e := eng.Load()
		if e == nil {
			return nil, nil
		}
		v, err := e.Would(ctx, info)
		if err != nil {
			return &agentturn.ToolDecision{Action: agentturn.Block, Reason: "the policy could not decide this call: " + err.Error()}, nil
		}
		var h *agentturn.ToolDecision
		if hook != nil {
			if h, err = hook(ctx, info); err != nil {
				return &agentturn.ToolDecision{Action: agentturn.Block, Reason: "a hook could not decide this call: " + err.Error()}, nil
			}
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
			return o.askChild(ctx, name, info, why, subject), nil
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

// askChild puts a sub-agent's call the policy or a hook asks about to
// the user, from inside the sub-agent's run.
func (o Options) askChild(ctx context.Context, name string, info agentturn.ToolCallInfo, why, subject string) *agentturn.ToolDecision {
	reason := "the " + name + " sub-agent asks: " + why
	if subject != "" && !strings.Contains(reason, subject) {
		reason += "; about: " + subject
	}
	cannot := &agentturn.ToolDecision{Action: agentturn.Block, Reason: "the " + name + " sub-agent cannot ask you (" + why + "); make this call yourself, so that it can be put to the user", By: agentpolicy.ByPolicy}
	if o.Ask != nil {
		allow, note, err := o.Ask(ctx, info.Call, reason)
		switch {
		case err != nil && ctx.Err() != nil:
			return &agentturn.ToolDecision{Action: agentturn.Block, Reason: "the run was cut off while the " + name + " sub-agent waited for your answer", By: agentpolicy.ByPolicy}
		case err != nil:
			return cannot
		case !allow:
			o.log("  ✗ %s denied (%s)", info.Call.Name, name)
			out := deniedOutput
			if note != "" {
				out += " Reason: " + note
			}
			return &agentturn.ToolDecision{Action: agentturn.Block, Reason: out, By: agentpolicy.ByHuman}
		}
		return &agentturn.ToolDecision{Action: agentturn.Allow, By: agentpolicy.ByHuman}
	}
	if o.Approve == nil {
		// A front with no way to put a question from inside a
		// sub-agent's run (the terminal client answers the calls a run
		// leaves pending, and a sub-agent's run is not the parent's)
		// refuses, and says what to do: make the call from the main
		// agent, where it can be asked.
		return cannot
	}
	if !o.Approve(info.Call, reason) {
		o.log("  ✗ %s denied (%s)", info.Call.Name, name)
		return &agentturn.ToolDecision{Action: agentturn.Block, Reason: deniedOutput, By: agentpolicy.ByHuman}
	}
	return &agentturn.ToolDecision{Action: agentturn.Allow, By: agentpolicy.ByHuman}
}
