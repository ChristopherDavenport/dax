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
	"os"
	"os/exec"
	"path"
	"path/filepath"
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
	"github.com/ChristopherDavenport/agenttool/mcpclient"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/compact"
	"github.com/ChristopherDavenport/agentturn/session"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/internal/config"
	"github.com/ChristopherDavenport/dax/internal/executor"
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
	// Executor runs the extensions' tools elsewhere, in `dax execute`
	// (DialExecutor), in place of building them in this process over a
	// workspace: their calls, facts and stamps are the executor's, and
	// the session records its workspace. Setting both Executor and
	// Workspace is an error. Every extension with Tools must be one the
	// executor runs, no extension's matcher may give a served tool's
	// subjects (the policy reads the executor only through the tools'
	// facts), and MCP servers are refused, since they would run on
	// this machine. The project's files are not read with one yet. It
	// is the caller's to close, after the session.
	Executor *Executor
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
	// MCP are stdio MCP servers started with the session, each offering
	// its tools as mcp__<Name>__<tool>. Each runs in the workspace when
	// it can start a process (workspace.Starter), at its root and with
	// its environment, and otherwise on this machine. Their stderr is
	// dax's.
	MCP []MCPServer
	// MaxReadBytes is the most bytes of a file a tool should read,
	// which extension.ToolEnv carries; zero is tool.DefaultMaxRead.
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
	// look like a credential's, unless PassEnv names it.
	KeyEnv string
	// Log receives dax's own notes: compactions, denials, skill grants.
	// nil discards them.
	Log func(format string, args ...any)

	// executor runs the extensions' tools in place of the session's
	// in-process one, which builds them over the workspace; the session
	// closes it. It is the seam the tests use to stand in for an
	// executor elsewhere.
	executor executor.Executor
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
// in-process human plane over it (Backend), dax's own controls
// (Controls), and the store it is written to.
type Session struct {
	Kit *agentkit.Kit

	opts    Options
	live    live
	env     []string            // what an MCP server outside the workspace starts with
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

	ag     *agentturn.Agent // the one agent, which the Turn drives
	detach func()
	asks   asks // the questions asked while a call runs

	mu      sync.Mutex
	lastEnd *agentturn.RunEnd // how the last run ended, for the engine
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

// withSubjects is the session's adjustment to the agent's configuration
// (kitbackend.WithConfig). The call's facts are pinned for the length
// of its decision (executor.Set.Pin), so the policy's subjects and the
// rewrite it runs with come from one reading of its claim. A call the
// policy defers has what the policy was asking about added to its
// reason, since the reason is the question every front shows: the rule,
// the secret path, the git config key, the part of a command line, from
// the verdict's subject.
func (s *Session) withSubjects(cfg agentturn.Config) agentturn.Config {
	inner := cfg.BeforeToolCall
	if inner == nil {
		return cfg
	}
	eng := s.Kit.Engine()
	set := s.set
	cfg.BeforeToolCall = func(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
		unpin := set.Pin(ctx, info)
		defer unpin()
		d, err := inner(ctx, info)
		if eng == nil || err != nil || d == nil || d.Action != agentturn.Defer || info.Call == nil {
			return d, err
		}
		if v, ok := eng.Deferred(info.RunID, info.Call.CallID); ok && v.Subject != "" && !strings.Contains(d.Reason, v.Subject) {
			d.Reason += "; about: " + v.Subject
		}
		return d, nil
	}
	return cfg
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
	ws, ownWS := o.Workspace, false
	if o.Executor != nil {
		switch {
		case ws != nil:
			return nil, errors.New("both an executor and a workspace: the executor's tools act in its own")
		case len(o.MCP) > 0:
			return nil, errors.New("MCP servers cannot run with an executor yet: they would run on this machine, not where the tools act; remove them or the executor")
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
	// An MCP server runs in the workspace when it can start one, and
	// otherwise on this machine, with this machine's environment
	// scrubbed (mcpServer).
	env := tool.DefaultEnv(o.PassEnv, o.KeyEnv)
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
		mdOpts, refused := agentsMDOptions(ws, dir, o.UserDir, o.AgentsMDGlobal)
		s.refused = append(s.refused, refused...)
		kopts = append(kopts, agentkit.WithAgentsMD(agentsMDPath, mdOpts))
		res, err := agentsmd.Chain(agentsMDPath, mdOpts)
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
	// A tool's question mid-call goes to whoever holds the Backend.
	kopts = append(kopts, agentkit.WithToolElicitor(agentpolicy.ByHuman, s.elicit))
	// Set whether or not a server is configured, since /mcp may add one.
	kopts = append(kopts, agentkit.WithMCPStderr(os.Stderr))
	for _, m := range o.MCP {
		prefix, err := mcpPrefix(m.Name)
		if err != nil {
			return nil, err
		}
		t, err := mcpServer(ws, m.Command, env)
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
	s.ag = agentturn.New(s.withSubjects(kit.Config()), kit.AgentOptions()...)
	// The kit's recorder subscribes first, so an entry is in the store
	// before any front sees the event that wrote it.
	s.detach = kit.Attach(s.ag)
	if rec := kit.Recorder(); rec != nil {
		// The inputs a stopped session accepted and no run took are
		// handed to the agent, which takes them after the next prompt.
		rec.Requeue(ctx, s.ag)
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
	x := s.x
	return session.WithEnv(func(context.Context) (*agentsession.EnvEntry, error) {
		d := x.Descriptor()
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
	if s.opts.Executor != nil {
		return "", errors.New("MCP servers cannot run with an executor yet: they would run on this machine, not where the tools act")
	}
	prefix, err := mcpPrefix(name)
	if err != nil {
		return "", err
	}
	t, err := mcpServer(s.ws, command, s.env)
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
	a := s.ag
	cfg := a.Config()
	cfg.Reasoning = o.reasoningFor(context.Background(), cfg.ModelName)
	return a.SetConfig(cfg)
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

// closeTools closes the executor of the extensions' tools, once.
func (s *Session) closeTools() {
	if s.x != nil {
		s.x.Close()
		s.x = nil
	}
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

// mcpServer is the transport of an MCP server started from a command
// line, split on spaces, with its diagnostics on dax's stderr. In a
// workspace that can start a process (workspace.Starter) the server
// runs there, at its root, with the workspace's environment, over the
// process's pipes; in one that cannot, it runs on this machine
// (mcpTransport), with env as its whole environment.
func mcpServer(ws workspace.Workspace, command string, env []string) (mcp.Transport, error) {
	if _, ok := ws.(workspace.Starter); !ok {
		return mcpTransport(command, env)
	}
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return nil, errors.New("empty command")
	}
	// A server's diagnostics are text from a program the model may have
	// chosen; they get the same cleaning as its output.
	return &startTransport{ws: ws, cmd: workspace.Command{Args: fields, Stream: render.CleanWriter(stderr)}}, nil
}

// startTransport starts an MCP server in a workspace when the client
// connects, and speaks to it over the process's standard input and
// output.
type startTransport struct {
	ws  workspace.Workspace
	cmd workspace.Command
}

func (t *startTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	// The server lives as long as the connection, not the context it
	// was connected under (a command's, say), as one the command
	// transport starts does; closing the connection ends it, and so
	// does closing the workspace.
	p, err := workspace.Start(context.WithoutCancel(ctx), t.ws, t.cmd)
	if err != nil {
		return nil, err
	}
	pipes := processPipes{p}
	return (&mcp.IOTransport{Reader: pipes, Writer: pipes}).Connect(ctx)
}

// processPipes is a started process's output to read and input to
// write. Closing it, as the connection does when it closes, ends the
// process the way the MCP stdio shutdown asks: its input closed, a
// moment to leave, then SIGTERM, then SIGKILL (Process.Close).
type processPipes struct{ p workspace.Process }

func (r processPipes) Read(b []byte) (int, error)  { return r.p.Stdout().Read(b) }
func (r processPipes) Write(b []byte) (int, error) { return r.p.Stdin().Write(b) }
func (r processPipes) Close() error                { return r.p.Close() }

// mcpTransport starts an MCP server on this machine from a command
// line, split on spaces, with env as its whole environment and its
// diagnostics on dax's stderr. The kit's own command transport would
// hand the server dax's environment, keys included.
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
// user, and runs rewritten too if they approve. With no one to ask, it
// is refused. A policy that is off governs nothing, the child included.
//
// The call's facts are pinned in set from the verdict to the hooks'
// rewrite (executor.Set.Pin), so the arguments it runs with come from
// the reading the verdict was decided on; the pin is let go before the
// question, so a person's slow answer holds nothing open.
func (s *Session) childPolicy(name string, eng *atomic.Pointer[agentpolicy.Engine], hook func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error), set *executor.Set) func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
	return func(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
		e := eng.Load()
		if e == nil {
			return nil, nil
		}
		unpin := set.Pin(ctx, info)
		defer unpin() // a no-op after the unpin below, which comes before the question
		v, err := e.Would(ctx, info)
		if err != nil {
			return &agentturn.ToolDecision{Action: agentturn.Block, Reason: "the policy could not decide this call: " + err.Error()}, nil
		}
		var h *agentturn.ToolDecision
		if hook != nil {
			h, err = hook(ctx, info)
		}
		unpin()
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
			return s.askChild(ctx, name, info, why, subject, args), nil
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
// whoever holds the session's Turn, from inside the sub-agent's run: a
// Question, answered with Reply. Its context is the call's, so an abort
// gives the question up. With nobody to ask, the call is refused, and
// the model is told to make it itself, where it can be put to the user.
// An approved call runs with args, the hooks' rewrite, when there is
// one, as an allowed call does and as the main agent's approved call
// does: a file tool's call carries the stamp of the facts it was asked
// about, and a stamp the model forged never reaches the tool.
func (s *Session) askChild(ctx context.Context, name string, info agentturn.ToolCallInfo, why, subject string, args json.RawMessage) *agentturn.ToolDecision {
	reason := "the " + name + " sub-agent asks: " + why
	if subject != "" && !strings.Contains(reason, subject) {
		reason += "; about: " + subject
	}
	r, err := s.asks.put(ctx, info.Call, reason)
	switch {
	case errors.Is(err, errNoOne):
		return &agentturn.ToolDecision{Action: agentturn.Block, Reason: "the " + name + " sub-agent cannot ask you (" + why + "); make this call yourself, so that it can be put to the user", By: agentpolicy.ByPolicy}
	case err != nil:
		return &agentturn.ToolDecision{Action: agentturn.Block, Reason: "the run was cut off while the " + name + " sub-agent waited for your answer", By: agentpolicy.ByPolicy}
	case !r.Accept:
		s.opts.log("  ✗ %s denied (%s)", info.Call.Name, name)
		out := deniedOutput
		if r.Note != "" {
			out += " Reason: " + r.Note
		}
		return &agentturn.ToolDecision{Action: agentturn.Block, Reason: out, By: agentpolicy.ByHuman}
	}
	return &agentturn.ToolDecision{Action: agentturn.Allow, Args: args, By: agentpolicy.ByHuman}
}
