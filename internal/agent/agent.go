// Package agent assembles one dex session with agentkit: dex's tools
// and prompt, the AGENTS.md chain, skills, memory, a confirmation
// policy, MCP servers, the explore child agent and compaction, recorded
// into an agentsession content-addressed store (RFC 0002). What the kit cannot express is
// wired here by hand beside it, each place saying why.
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/ChristopherDavenport/agentmemory/filestore"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agentsmd"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agenttool/mcpclient"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/compact"
	"github.com/ChristopherDavenport/agentturn/session"
	childagent "github.com/ChristopherDavenport/agentturn/tools/agent"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ChristopherDavenport/dex/internal/config"
	"github.com/ChristopherDavenport/dex/internal/policy"
	"github.com/ChristopherDavenport/dex/internal/prompt"
	"github.com/ChristopherDavenport/dex/internal/render"
	"github.com/ChristopherDavenport/dex/internal/tool"
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
	// Streamer is the model; internal/provider builds one from the
	// configuration.
	Streamer openresponses.Streamer
	// Model is the model name the requests carry.
	Model string
	// SubagentModel is the model name the sub-agents' requests carry;
	// empty is Model.
	SubagentModel string
	// Fit is the reasoning effort to ask a model for in place of the
	// one Think implies, from what its vendor says it takes; nil asks
	// as configured. It is applied where each configuration is built,
	// never to a request on its way out, so the session records the
	// request that was sent.
	Fit   func(ctx context.Context, model string, want openresponses.ReasoningEffort) openresponses.ReasoningEffort
	Think bool
	// Effort is the reasoning effort Think asks for; empty is low.
	Effort openresponses.ReasoningEffort
	// Dir is the working directory the tools and prompt are rooted at.
	Dir string
	// Root is the session store, a content-addressed store holding
	// every project's sessions; empty disables recording.
	Root string
	// Sync is when an append to the store is durable before it
	// returns: every append (the default), a response or a call's
	// output and what came before it, or at Close.
	Sync cas.SyncPolicy
	// UserDir is the user's dex directory, ~/.dex: its AGENTS.md is
	// read before the project's and its skills directory searched
	// after the project's.
	UserDir string
	// Instructions is text of the user's own, from the config's
	// instructions_file, added to dex's part of the system prompt.
	Instructions string

	// AgentsMD reads UserDir/AGENTS.md and the AGENTS.md chain from the
	// file system root down to Dir into the instructions.
	AgentsMD bool
	// Skills offers the skills under Dir/.dex/skills and
	// UserDir/skills, through the skill tool, and those under
	// SkillsDirs.
	Skills bool
	// SkillsDirs are configured skill directories. One that does not
	// exist is an error, unlike the two default ones.
	SkillsDirs []string
	// TrustSkills lets a skill's allowed-tools widen the confirmation
	// policy while the model follows it, until the next user message.
	TrustSkills bool
	// MemoryDir is the memory store; empty disables memory. The model
	// reads and writes a user scope and a scope for Dir.
	MemoryDir string

	// Compact, when positive, is the estimated token budget above which
	// the transcript is folded before a call.
	Compact int
	// CompactServer folds through the server's compaction endpoint
	// (compact.New) instead of a local summary (compact.NewLocal).
	CompactServer bool
	// Policy decides which calls run, ask or are refused; nil lets
	// every call run. internal/policy builds dex's.
	Policy *agentpolicy.Policy
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
	// its tools as mcp__<Name>__<tool>. Their stderr is dex's.
	MCP []MCPServer
	// MaxReadBytes is the most bytes of a file the read tool scans and
	// the edit tool will rewrite; zero is tool.DefaultMaxRead.
	MaxReadBytes int64
	// PassEnv names credential-looking variables bash commands and MCP
	// servers may still inherit; every other credential is removed from
	// their environment.
	PassEnv []string
	// KeyEnv is the variable the provider's key was read from. It is
	// removed from the environment of bash commands and MCP servers
	// even when its name does not look like a credential's, unless
	// PassEnv names it.
	KeyEnv string
	// NoAgent builds the kit and no agent over it: a front that drives
	// the kit through its own backend (the terminal client) builds the
	// agent itself, and two agents on one recorder would both write
	// the session. Session.Agent is nil, and Prompt, Steer and the
	// other methods that drive it must not be called.
	NoAgent bool
	// Agents offers the sub-agents as tools: explore, read-only, and
	// task, which changes files; both run on SubagentModel.
	Agents bool
	// Log receives dex's own notes: compactions, denials, skill grants.
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

// DefaultUserDir is ~/.dex.
func DefaultUserDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".dex"
	}
	return filepath.Join(home, ".dex")
}

// DefaultRoot is ~/.dex/sessions.
func DefaultRoot() string { return filepath.Join(DefaultUserDir(), "sessions") }

// Session is one dex conversation: the kit that assembled it, the
// agent running it and, when recording, the store it is written to.
type Session struct {
	Agent *agentturn.Agent
	Kit   *agentkit.Kit

	opts    Options
	live    live
	env     []string            // what bash and MCP servers start with
	refused []agentkit.Omission // repository files screened out before the kit
	ws      *tool.Workspace
	store   *cas.Store
	detach  func()
}

// live is what /think and /model have set, read by each sub-agent
// call, which may run while the main agent's settings change.
type live struct {
	mu    sync.Mutex
	think bool
	main  string
}

// now is the Think setting and the main model in force, and the
// sub-agent model: the configured one, or the main model when none is.
func (s *Session) now() (think bool, main, sub string) {
	s.live.mu.Lock()
	think, main = s.live.think, s.live.main
	s.live.mu.Unlock()
	sub = s.opts.SubagentModel
	if sub == "" {
		sub = main
	}
	return think, main, sub
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

func (o Options) subagentModel() string {
	if o.SubagentModel != "" {
		return o.SubagentModel
	}
	return o.Model
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

// ProjectScope is the memory scope of a working directory. One store
// holds every project's memory beside the user's, so each project gets
// a scope of its own, named for its directory and a hash of its path.
func ProjectScope(dir string) agentmemory.Scope {
	var b strings.Builder
	b.WriteString("project-")
	for _, r := range strings.ToLower(filepath.Base(dir)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
		if b.Len() >= 40 {
			break
		}
	}
	if !strings.HasSuffix(b.String(), "-") {
		b.WriteByte('-')
	}
	sum := sha256.Sum256([]byte(dir))
	b.WriteString(hex.EncodeToString(sum[:4]))
	return agentmemory.Scope(b.String())
}

// explore is the child agent: a read-only investigator on a fresh
// transcript whose final answer comes back to the parent as the tool
// output.
func (o Options) explore(ctx context.Context, model openresponses.Streamer, ws *tool.Workspace, env []string, eng *atomic.Pointer[agentpolicy.Engine]) agentturn.Config {
	cfg := agentturn.Config{
		Name: "explore",
		Description: "Delegate a read-only investigation of the project to a sub-agent. " +
			"Give it one clear question; it reads files and runs read-only commands and returns a written answer. " +
			"Use it for broad searches so their output stays out of this conversation.",
		Model:     model,
		ModelName: o.subagentModel(),
		Instructions: "You are a read-only explorer working in " + o.Dir + ". Answer the question using the read, glob, grep, ls and bash tools; " +
			"never modify files. End with a concise written answer that stands on its own.",
		Tools:     append(tool.ReadOnly(ws, o.MaxReadBytes), tool.Bash(ws.Dir(), tool.WithEnv(env))),
		Reasoning: o.reasoningFor(ctx, o.subagentModel()),
		MaxTurns:  10,
		Retry:     agentturn.Retry{MaxAttempts: 3},
	}
	if o.Policy != nil {
		// The child is governed by the parent's policy, the same engine
		// and rules for every tool: the user's denies, the secret-path
		// asks, the path rules. The engine does not exist until
		// agentkit.New has built it, after this config is fixed, so
		// the hook reads it through eng, which open fills in. A call the
		// policy asks about is put to the user, through the same Approve
		// the parent's calls go through, from inside the child's run.
		cfg.BeforeToolCall = o.childPolicy("explore", eng, &tool.Analyzer{Dir: o.Dir, MaxFile: o.MaxReadBytes})
	}
	return cfg
}

// open assembles the kit and the agent. resume, when not empty, is the
// session to continue; the store is nil when nothing is recorded.
func open(ctx context.Context, o Options, store *cas.Store, resume string) (*Session, error) {
	if o.Streamer == nil {
		return nil, errors.New("no model")
	}
	ws, err := tool.NewWorkspace(o.Dir)
	if err != nil {
		return nil, fmt.Errorf("workspace: %w", err)
	}
	s := &Session{opts: o, store: store, ws: ws}
	s.live.think, s.live.main = o.Think, o.Model
	ok := false
	defer func() {
		if !ok {
			ws.Close()
		}
	}()
	model := o.Streamer
	var engine atomic.Pointer[agentpolicy.Engine]
	env := tool.DefaultEnv(o.PassEnv, o.KeyEnv)
	s.env = env
	kopts := []agentkit.Option{
		agentkit.WithName("dex", "A coding agent that reads, writes and edits files and runs shell commands in a project."),
		agentkit.WithModel(model, o.Model),
		agentkit.WithReasoning(o.reasoningFor(ctx, o.Model)),
		agentkit.WithRetry(agentturn.Retry{MaxAttempts: 3}),
		agentkit.WithInstructions(prompt.Build(o.Dir, o.Instructions)),
		agentkit.WithTools(tool.Builtins(ws, o.MaxReadBytes, tool.WithEnv(env))...),
	}
	// agentsText is the AGENTS.md part as the kit renders it, for the
	// task sub-agent, whose configuration is fixed before the kit is
	// built. The kit has no instruction budget, under which it renders
	// the chain whole, so the same options give the same text.
	agentsText := ""
	if o.AgentsMD {
		// The walk is done here, so that a file that is a link out of
		// the workspace can be left out; agentsmd is given the screened
		// files and a name that matches nothing, so it walks to no more.
		files, refused := agentsFiles(o.Dir)
		s.refused = append(s.refused, refused...)
		mdOpts := agentsmd.Options{
			Names:  []string{".dex-no-such-file"},
			Extra:  append([]string{filepath.Join(o.UserDir, "AGENTS.md")}, files...),
			Budget: 32 << 10,
		}
		kopts = append(kopts, agentkit.WithAgentsMD(o.Dir, mdOpts))
		if o.Agents {
			res, err := agentsmd.Chain(o.Dir, mdOpts)
			if err != nil {
				return nil, fmt.Errorf("AGENTS.md: %w", err)
			}
			agentsText = agentsmd.Render(res.Files)
		}
	}
	if o.Skills {
		// Neither directory is one the user configured, so either may
		// be absent; the project's comes first and shadows the user's.
		dirs := []string{filepath.Join(o.UserDir, "skills")}
		if ok, refused := projectSkillsOK(o.Dir); ok {
			dirs = []string{filepath.Join(o.Dir, ".dex", "skills"), dirs[0]}
		} else {
			s.refused = append(s.refused, refused...)
		}
		kopts = append(kopts, agentkit.WithOptionalSkills(dirs...))
		if len(o.SkillsDirs) > 0 {
			kopts = append(kopts, agentkit.WithSkills(o.SkillsDirs...))
		}
		if o.TrustSkills {
			kopts = append(kopts,
				agentkit.WithSkillGrants(func(sk *agentskill.Skill) agentpolicy.Source {
					// Only a skill from a directory the user named is
					// trusted: ~/.dex/skills and the config's skills_dirs.
					// A repository's skill is text from the repository; its
					// allowed-tools are withheld like any untrusted rule.
					return agentpolicy.Source{Name: "skill:" + sk.ListedName(), Path: sk.Location, Trusted: userSkill(o, sk.Location)}
				}),
				agentkit.WithSkillGrantScope(),
				agentkit.WithSkillGrantReport(func(g agentkit.SkillGrant) {
					o.log("[skill %s: granted %v, refused %d, err %v]", g.Skill, g.Granted, len(g.Refused), g.Err)
				}))
		}
	}
	if o.MemoryDir != "" {
		if err := secureDir(o.MemoryDir); err != nil {
			return nil, fmt.Errorf("memory: %w", err)
		}
		mem, err := filestore.Open(o.MemoryDir)
		if err != nil {
			return nil, fmt.Errorf("memory: %w", err)
		}
		kopts = append(kopts, agentkit.WithMemory(mem, "user", ProjectScope(o.Dir)))
	}
	if o.Policy != nil {
		kopts = append(kopts, agentkit.WithPolicy(*o.Policy, policy.Matchers(o.Dir, o.MaxReadBytes), policy.Options()...),
			// A bash call the policy allows without asking carries the
			// plan it approved, and runs only that plan.
			agentkit.WithBeforeToolCall(stampBash(&tool.Analyzer{Dir: o.Dir, MaxFile: o.MaxReadBytes})))
	}
	if o.Agents {
		kopts = append(kopts,
			agentkit.WithChildAgent(o.explore(ctx, model, ws, env, &engine), childagent.WithCallConfig(s.exploreCall)),
			agentkit.WithChildAgent(o.task(ctx, model, ws, env, &engine, agentsText), childagent.WithCallConfig(s.taskCall)))
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
			kopts = append(kopts, agentkit.WithResumedSession(store, resume, o.env()))
		} else {
			kopts = append(kopts, agentkit.WithSession(store, o.header(), o.env()))
		}
	}

	kit, err := agentkit.New(ctx, kopts...)
	if err != nil {
		return nil, err
	}
	s.Kit = kit
	if e := kit.Engine(); e != nil {
		engine.Store(e)
	}
	if !o.NoAgent {
		s.Agent = agentturn.New(kit.Config(), kit.AgentOptions()...)
		s.detach = kit.Attach(s.Agent)
	}
	ok = true
	return s, nil
}

func (o Options) header() agentsession.Header {
	return agentsession.Header{
		CWD:     o.Dir,
		Harness: &agentsession.Harness{Name: "dex", Version: Version},
	}
}

// env is the environment entry the recorder asks for once per run: the
// directory the tools are rooted at, on this machine's file system. The
// recorder writes it only when it differs from the last one on the
// path, so a session that stays in one directory holds one entry and a
// resume in another directory says so.
func (o Options) env() session.Option {
	return session.WithEnv(func(context.Context) (*agentsession.EnvEntry, error) {
		e := agentsession.NewEnvEntry(o.Dir)
		e.SetWorkspace(agentsession.WorkspaceLocal, "")
		return e, nil
	})
}

// New starts a fresh session.
func New(ctx context.Context, o Options) (*Session, error) {
	if o.Root == "" {
		return open(ctx, o, nil, "")
	}
	store, err := openStore(o.Root, cas.WithSync(o.Sync))
	if err != nil {
		return nil, err
	}
	s, err := open(ctx, o, store, "")
	if err != nil {
		store.Close()
		return nil, err
	}
	return s, nil
}

// Resume reopens a recorded session and continues it from its leaf.
// Calls an earlier run left unanswered become pending on the agent,
// each with the reason the file's dispatch records give it, and are
// answered by the next Prompt.
func Resume(ctx context.Context, o Options, id string) (*Session, error) {
	if o.Root == "" {
		return nil, errors.New("resume needs a session store")
	}
	store, err := openStore(o.Root, cas.WithSync(o.Sync))
	if err != nil {
		return nil, err
	}
	s, err := open(ctx, o, store, id)
	if err != nil {
		store.Close()
		return nil, err
	}
	sess := s.Kit.Session()
	if t := sess.Truncated(); t != nil {
		fmt.Fprintf(stderr, "dex: session %s: %v (dropped)\n", id, t)
	}
	if f := sess.DeclaredFormat(); f != agentsession.Format {
		// The recorder raises the header before its first append, after
		// which no reader older than agentsession v0.0.18 opens it.
		fmt.Fprintf(stderr, "dex: session %s was written as %s; this dex writes %s, and readers before agentsession v0.0.18 refuse it from the next entry\n", id, f, agentsession.Format)
	}
	if cwd := sess.Header().CWD; cwd != "" && cwd != o.Dir {
		fmt.Fprintf(stderr, "dex: session was recorded in %s, continuing in %s\n", cwd, o.Dir)
	}
	return s, nil
}

// ID is the session ID, empty when not recording.
func (s *Session) ID() string { return s.Kit.SessionID() }

// Path is the session's directory in the store, its header, log and
// head, empty when not recording.
func (s *Session) Path() string {
	if s.store == nil {
		return ""
	}
	return filepath.Join(s.store.Root(), "sessions", s.ID())
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
	// Memory written during the run names this session; the kit cannot
	// put the ID on a context that is the host's.
	ctx = agentmemory.WithSession(ctx, s.ID())
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
	think, _, _ := s.now()
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
	s.ws.Close()
	if s.store != nil {
		if cerr := s.store.Close(); err == nil {
			err = cerr
		}
	}
	return err
}

// readOnly opens the store for reading, without the session locks, so
// a session a running dex holds can be listed, verified and projected.
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

// Import reads an RFC 0001 JSONL session, one an earlier dex wrote,
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
// removed. A dex holding a session may keep writing meanwhile.
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
// dex's stderr. The kit's own command transport would hand the server
// dex's environment, keys included.
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

// userSkill reports whether a skill's location is under a directory
// the user chose: UserDir/skills or one of Options.SkillsDirs. The
// project's .dex/skills is not, nor is anything else.
func userSkill(o Options, location string) bool {
	roots := append([]string{filepath.Join(o.UserDir, "skills")}, o.SkillsDirs...)
	loc := filepath.Clean(location)
	if real, err := filepath.EvalSymlinks(loc); err == nil {
		loc = real
	}
	for _, r := range roots {
		if real, err := filepath.EvalSymlinks(r); err == nil {
			r = real
		}
		if rel, err := filepath.Rel(r, loc); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return true
		}
	}
	return false
}

// stampBash is the hook that stamps an auto-allowed bash call with the
// plan the policy approved, and takes a stamp the model made off any
// other. It decides nothing itself.
func stampBash(an *tool.Analyzer) func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
	return func(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
		if info.Call == nil || info.Call.Name != "bash" {
			return nil, nil
		}
		args, changed, err := tool.StampArgs(ctx, an, info.Args)
		if err != nil || !changed {
			return nil, nil // not JSON: the tool will say so
		}
		return &agentturn.ToolDecision{Action: agentturn.Allow, Args: args}, nil
	}
}

// childPolicy decides a call of the explore child under the parent's
// policy: a verdict of Block blocks it, Allow lets it run (a bash line
// the policy auto-allows, stamped like the parent's), and a call the
// policy asks about is put to the user. With no one to ask, it is
// refused. A policy that is off governs nothing, the child included.
func (o Options) childPolicy(name string, eng *atomic.Pointer[agentpolicy.Engine], an *tool.Analyzer) func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
	return func(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
		e := eng.Load()
		if e == nil {
			return nil, nil
		}
		v, err := e.Would(ctx, info)
		if err != nil {
			return &agentturn.ToolDecision{Action: agentturn.Block, Reason: "the policy could not decide this call: " + err.Error()}, nil
		}
		switch v.Action {
		case agentturn.Block:
			return &agentturn.ToolDecision{Action: agentturn.Block, Reason: v.Reason, By: agentpolicy.ByPolicy}, nil
		case agentturn.Defer:
			reason := "the " + name + " sub-agent asks: " + v.Reason
			if v.Subject != "" && !strings.Contains(reason, v.Subject) {
				reason += "; about: " + v.Subject
			}
			if o.Ask != nil {
				allow, note, err := o.Ask(ctx, info.Call, reason)
				switch {
				case err != nil && ctx.Err() != nil:
					return &agentturn.ToolDecision{Action: agentturn.Block, Reason: "the run was cut off while the " + name + " sub-agent waited for your answer", By: agentpolicy.ByPolicy}, nil
				case err != nil:
					return &agentturn.ToolDecision{Action: agentturn.Block, Reason: "the " + name + " sub-agent cannot ask you (" + v.Reason + "); make this call yourself, so that it can be put to the user", By: agentpolicy.ByPolicy}, nil
				case !allow:
					o.log("  ✗ %s denied (%s)", info.Call.Name, name)
					out := deniedOutput
					if note != "" {
						out += " Reason: " + note
					}
					return &agentturn.ToolDecision{Action: agentturn.Block, Reason: out, By: agentpolicy.ByHuman}, nil
				}
				return &agentturn.ToolDecision{Action: agentturn.Allow, By: agentpolicy.ByHuman}, nil
			}
			if o.Approve == nil {
				// A front with no way to put a question from inside a
				// sub-agent's run (the terminal client answers the
				// calls a run leaves pending, and a sub-agent's run is
				// not the parent's) refuses, and says what to do: make
				// the call from the main agent, where it can be asked.
				return &agentturn.ToolDecision{Action: agentturn.Block, Reason: "the " + name + " sub-agent cannot ask you (" + v.Reason + "); make this call yourself, so that it can be put to the user", By: agentpolicy.ByPolicy}, nil
			}
			if !o.Approve(info.Call, reason) {
				o.log("  ✗ %s denied (%s)", info.Call.Name, name)
				return &agentturn.ToolDecision{Action: agentturn.Block, Reason: deniedOutput, By: agentpolicy.ByHuman}, nil
			}
			return &agentturn.ToolDecision{Action: agentturn.Allow, By: agentpolicy.ByHuman}, nil
		}
		// The verdict is returned rather than left implicit, so the
		// sub-agent's session records a decision for each call, as the
		// main agent's does through the engine's observer.
		d := &agentturn.ToolDecision{Action: agentturn.Allow, Reason: v.Reason, By: agentpolicy.ByPolicy}
		if info.Call != nil && info.Call.Name == "bash" {
			if args, changed, err := tool.StampArgs(ctx, an, info.Args); err == nil && changed {
				d.Args = args
			}
		}
		return d, nil
	}
}
