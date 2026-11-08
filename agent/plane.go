package agent

// This file is the session's human or autonomous plane: the session as
// agentturn's control contract, which a front, a controller or a wire
// drives, dax's own controls beside it, and Drive, a controller that
// answers by rule.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/internal/render"
)

// ErrHeld is what Prompt returns while calls a stopped session held for
// approval wait for an answer: nobody has said whether they may run, so
// they are answered first, with Resume, and the prompt goes in as a
// steer of that run (Drive does both).
var ErrHeld = errors.New("calls held for approval when the session stopped wait for an answer")

// The Session is its agent as agentturn's control contract, the human
// or autonomous plane a front, a controller that answers by rule
// (Drive) or a wire holds: the kit's control of the agent
// (agentkit.Control: the run context, the engine's release of held
// calls, the queued entries, the questions as events, the head move)
// with dax's own rules as a controller on top:
//
//   - Prompt answers the calls an abort or a restart left unanswered
//     ahead of its items, with what is known of them (cut off and may
//     have run; never reached its tool), in the same model call, and
//     refuses with ErrHeld while calls a stopped session held for
//     approval wait;
//   - Resume answers a pending call no one waits on (one an abort cut
//     off) the same way, beside the answers it is given;
//   - a question no front here can answer is not asked: a form (but
//     dax's own question for text, extension.TextQuestion, which the
//     fronts answer with a line) or a page to visit is cancelled, and
//     noted, and with no front
//     answering questions (Answering) a tool's question is cancelled and
//     a sub-agent's call the policy asks about stays deferred, so its
//     task call fails input-required (agentturn RFC 0001 Composition).
//
// Questions asked while a call runs are agentturn.Question events,
// answered with Reply; what the user says with an answer is its Note.
var _ agentturn.Control = (*Session)(nil)

// Permission is a call a run left for a person to answer.
type Permission struct {
	Call *openresponses.FunctionCall
	// Reason is the policy's question, with what it was asking about:
	// the rule, the secret path, the git config key, the part of a
	// command line.
	Reason string
}

// Agent is the session's one agent, for what the control contract does
// not carry: its configuration. Drive the session through the Session
// itself, whose rules a run on the agent would go around.
func (s *Session) Agent() *agentturn.Agent { return s.ag }

// Record is the store the session is written to, as a view follows it
// (Follow from Info's ID), or nil for a store that cannot be followed.
// A session nothing is kept of is written to memory and followed the
// same way.
func (s *Session) Record() agentsession.Follower {
	f, _ := s.store.(agentsession.Follower)
	return f
}

// Recorder is the recorder writing the session, for a view that follows
// it (agentconsole's client.RecordOf).
func (s *Session) Recorder() *session.Recorder { return s.Kit.Recorder() }

// RunContext puts on ctx what the kit expects of a run it records: its
// recorder, the session's ID for the model calls, the session for
// memory a run writes and the reasoning attribution of the branch the
// head was last moved to (agentkit.Control.RunContext). Prompt and
// Resume use it.
func (s *Session) RunContext(ctx context.Context) context.Context { return s.ctl.RunContext(ctx) }

// Prompt implements agentturn.Control, answering the calls an abort or
// a restart left unanswered first and refusing with ErrHeld while held
// calls wait.
func (s *Session) Prompt(ctx context.Context, items ...openresponses.Item) (*agentturn.RunEnd, error) {
	if slices.ContainsFunc(s.ag.State().Pending, held) {
		return nil, ErrHeld
	}
	// The model a mode switch asked for (plan_model), then the
	// extensions' BeforePrompt hooks, the person being here.
	s.modelBeforeRun()
	s.promptHooks(ctx)
	pending := s.ag.State().Pending
	outs := make([]openresponses.Item, 0, len(pending)+len(items)+1)
	for _, p := range pending {
		outs = append(outs, answerOf(p))
	}
	return s.ctl.Prompt(ctx, append(outs, items...)...)
}

// Resume implements agentturn.Control: the answers go through the
// kit's engine, which releases the calls it only held beside the ones
// asked about, and a pending call no one waits on is answered with
// what is known of it.
func (s *Session) Resume(ctx context.Context, answers ...agentturn.Answer) (*agentturn.RunEnd, error) {
	given := map[string]bool{}
	for _, a := range answers {
		given[a.CallID] = true
	}
	s.modelBeforeRun()
	s.actModel(answers)
	for _, p := range s.ag.State().Pending {
		if !held(p) && !given[p.Call.CallID] {
			answers = append(answers, agentturn.Output(answerOf(p)))
		}
	}
	return s.ctl.Resume(ctx, answers...)
}

// Permissions are the calls a run that ended for input left for a
// person, each with the policy's question and the part of the call it
// is about: not the calls the engine holds beside them, which Resume
// releases.
func (s *Session) Permissions(end *agentturn.RunEnd) []Permission {
	var out []Permission
	for _, p := range s.ctl.Permissions(end) {
		out = append(out, Permission{Call: p.Call, Reason: about(p.Reason, p.Subject)})
	}
	return out
}

// about adds to a question the part of the call it is about, unless it
// names it already.
func about(reason, subject string) string {
	if subject == "" || strings.Contains(reason, subject) {
		return reason
	}
	if reason == "" {
		return "about: " + subject
	}
	return reason + "; about: " + subject
}

// Queue implements agentturn.Control: each item is on the record, as a
// queued entry, before it returns.
func (s *Session) Queue(ctx context.Context, mode agentturn.QueueMode, items ...openresponses.Item) error {
	return s.ctl.Queue(ctx, mode, items...)
}

// Deliver implements agentturn.Deliverer: the items join the run in
// flight, or start the run that takes them, each on the record as a
// queued entry first (agentkit's Control.Deliver). While calls wait on
// a person it starts none (agentturn.ErrInputRequired), and the Resume
// that answers them takes the items, so a delivery never settles what
// the policy held. A view steers through it without waiting for the
// next prompt.
func (s *Session) Deliver(ctx context.Context, items ...openresponses.Item) (bool, *agentturn.RunEnd, error) {
	return s.ctl.Deliver(ctx, items...)
}

// Accept implements agentturn.Accepter: Deliver in two steps, returning
// once the items are queued, so an attached view's steer is taken while
// a quiet tool runs rather than held behind it (agentconsole's
// client.Direct sends a steer through it). The run it may start goes on
// ctx's values without its cancellation, and only Abort ends it.
func (s *Session) Accept(ctx context.Context, items ...openresponses.Item) (*agentturn.Delivery, error) {
	return s.ctl.Accept(ctx, items...)
}

var (
	_ agentturn.Deliverer = (*Session)(nil)
	_ agentturn.Accepter  = (*Session)(nil)
)

// Abort implements agentturn.Control.
func (s *Session) Abort() { s.ctl.Abort() }

// State implements agentturn.Control.
func (s *Session) State() agentturn.State { return s.ctl.State() }

// Subscribe implements agentturn.Control. The recorder subscribed
// first, so an entry is in the store before a subscriber sees the event
// that wrote it.
func (s *Session) Subscribe(fn func(context.Context, agentturn.Event) error) func() {
	return s.ctl.Subscribe(fn)
}

// Reply implements agentturn.Control: it answers a question asked
// while a call runs. A question no longer waiting is an error. Who
// answered is the answerer ctx names (agentturn.ContextWithAnswerer),
// which the record writes beside the answer: a front names the person
// it took the answer from (AtTerminal), and a reply that names nobody
// is unattributed. The run's own context, which a Question event is
// delivered with, names who started the run, not who answers.
func (s *Session) Reply(ctx context.Context, id string, answer agenttool.Answer) error {
	return s.ctl.Reply(ctx, id, answer)
}

// AtTerminal is the person at this terminal, as who answered a
// question: a front that takes the answer from them names it on
// Reply's context with agentturn.ContextWithAnswerer.
var AtTerminal = agentturn.Trigger{Kind: "human", Source: "terminal"}

// ContinueFrom moves the session's head to the entry: the next prompt
// continues the conversation from there, on a branch of the session,
// with the skill grants of that branch (agentkit.Control.ContinueFrom),
// and the branch's unclaimed prompts kept through a fold, as a resume
// keeps them.
func (s *Session) ContinueFrom(ctx context.Context, entryID string) error {
	if err := s.ctl.ContinueFrom(ctx, entryID); err != nil {
		return err
	}
	s.claimUnclaimed()
	return nil
}

// Answering says a front answers the questions asked while a call
// runs, until the returned func is called. With none answering, a
// question is not asked: a tool's is cancelled, and a sub-agent's call
// the policy asks about stays deferred, its task call failing
// input-required, naming the call, for the main agent's model.
func (s *Session) Answering() (done func()) {
	s.answering.Add(1)
	var once sync.Once
	return func() { once.Do(func() { s.answering.Add(-1) }) }
}

// ask puts a question to the fronts answering, as an agentturn.Question
// event, naming the A2A caller's conversation it comes from, if any
// (forPeer); errNoOne when none is, and for an A2A caller's question
// when no view is attached, or when the last one goes while it waits
// (peerAsk).
func (s *Session) ask(ctx context.Context, q agenttool.Elicitation) (agenttool.Answer, error) {
	if s.answering.Load() == 0 {
		return agenttool.Answer{}, errNoOne
	}
	ctx, done, ok := s.peerAsk(ctx)
	if !ok {
		return agenttool.Answer{}, errNoOne
	}
	defer done()
	q.Message = forPeer(ctx, q.Message)
	return orNoOne(ctx)(s.ag.QuestionElicitor()(ctx, q))
}

// errNoOne is a question asked with no front answering questions.
var errNoOne = errors.New("nobody is answering questions")

// elicit is the session's ToolElicitor: a tool's question goes to the
// fronts answering as a question event. dax's own question for a line
// of text (extension.TextQuestion) the fronts answer with what the user
// typed; any other form, or a page to visit, is one no front here can
// show, so it is cancelled, and noted.
func (s *Session) elicit(ctx context.Context, q agenttool.Elicitation) (agenttool.Answer, error) {
	text := extension.IsTextQuestion(q)
	switch {
	case q.URL != "":
		s.opts.log("[a tool asks you to visit %s: %s]", render.Clean(q.URL), render.Clean(q.Message))
		return agenttool.Answer{Action: agenttool.ActionCancel}, nil
	case hasFields(q.Schema) && !text:
		s.opts.log("[a tool asks for a form dax cannot show: %s]", render.Clean(q.Message))
		return agenttool.Answer{Action: agenttool.ActionCancel}, nil
	}
	// A call an A2A caller's conversation asks about (front/a2a puts
	// it with agentturn.Ask) is the policy's question, not a tool's; so
	// is a sub-agent's call (tools/agent's WithAskParent puts it), whose
	// question is dax's own text, the sub-agent and the policy's
	// reason, as before agentturn.Ask framed it.
	call, asked := agentturn.AskedCallFrom(ctx)
	switch {
	case asked && call.Parent == "" && call.Decision != nil && isChildAsk(call.Decision.Reason):
		q.Message = call.Decision.Reason
		// The fronts show the call as the sub-agent made it: the stamp
		// its facts claim added for the run is dax's, not the call's.
		call.Args = unstamped(call.Args)
		ctx = agentturn.ContextWithAskedCall(ctx, call)
	case !asked || call.Parent != "":
		q.Message = "a tool asks: " + q.Message
	}
	ans, err := s.ask(ctx, q)
	switch {
	case err != nil:
		return agenttool.Answer{Action: agenttool.ActionCancel}, nil
	case ans.Action == agenttool.ActionAccept && text:
		// A front that answers a text question with a note (the
		// terminal client's) gave the answer as the note: it is the
		// form's field, said once, and not a note besides.
		reply, _ := extension.TextAnswer(q, ans)
		if reply == strings.TrimSpace(ans.Note) {
			ans.Note = ""
		}
		ans.Content = extension.TextContent(reply)
	case ans.Action == agenttool.ActionAccept && ans.Content == nil:
		// A yes asks for no field, and MCP wants the content anyway.
		ans.Content = json.RawMessage(`{}`)
	}
	return ans, nil
}

// hasFields reports whether a form's schema asks for any property.
func hasFields(schema json.RawMessage) bool {
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	return len(schema) > 0 && json.Unmarshal(schema, &s) == nil && len(s.Properties) > 0
}

// Controls are dax's own controls of a session, beside agentturn's
// control contract: what a front's slash commands do and what its start
// lines show. They are a product's, not every agent's, so they are not
// part of the contract: the model and the reasoning setting, the MCP
// servers, and the session's assembly. A wire to a remote front carries
// them beside it (front/control's commands); the Session is their
// in-process implementation.
type Controls interface {
	// SetModel changes the model for later runs, sub-agents included.
	SetModel(name string) error
	// SetThink turns reasoning on or off for later runs.
	SetThink(on bool) error
	// AddMCP starts a stdio MCP server whose tools are offered from the
	// next run as mcp__<name>__<tool>, returning the label RemoveMCP
	// takes.
	AddMCP(ctx context.Context, name, command string) (string, error)
	// RemoveMCP closes a server AddMCP started.
	RemoveMCP(label string) error
	// Info is what the session is: its ID, where it is kept, the tools
	// it was assembled with, what its instruction layers left out and
	// its MCP servers.
	Info() Info
}

// Info describes a session for a front's start lines.
type Info struct {
	// ID is the session's ID. Recorded is false for a session nothing
	// is kept of (no store), whose ID names nothing after it closes.
	ID       string
	Recorded bool
	// Name is its name (/name), empty for none.
	Name string `json:",omitempty"`
	// Path is its directory in a local store; empty otherwise.
	Path    string
	Tools   []agentkit.ToolOrigin
	Omitted []agentkit.Omission
	// AgentsMDBudget is the bound in bytes the AGENTS.md chain was read
	// under, 0 when it was not read; AgentsMDProject is the project's
	// own bound (its agents_md_budget) when that was the tighter one,
	// so a file Omitted over budget was the project's to leave out.
	AgentsMDBudget  int64 `json:",omitempty"`
	AgentsMDProject int64 `json:",omitempty"`
	// Workspace says where its tools act: the kind, the ref and the
	// root.
	Workspace workspace.Descriptor
	// MCP are its MCP servers, each with where it runs and what it is
	// for, listed the same wherever it runs.
	MCP []MCPServerInfo
	// Loaded are the held-back tools the person's conversation has
	// loaded with tool_search, in the order it loaded them; a tool in
	// Tools with Deferred set and not here is listed to the model by
	// name alone.
	Loaded []string `json:",omitempty"`
	// Policy is the policy in force, nil for a session with none.
	Policy *PolicyInfo
}

var _ Controls = (*Session)(nil)

// Info implements Controls.
func (s *Session) Info() Info {
	return Info{ID: s.ID(), Recorded: s.recorded, Name: s.Name(), Path: s.Path(), Tools: s.Tools(), Omitted: s.Omitted(), AgentsMDBudget: s.agentsMDBudget, AgentsMDProject: s.agentsMDProject, Workspace: s.ws.Descriptor(), MCP: s.mcpServers(), Loaded: s.loadedTools(), Policy: s.policyInfo()}
}

// Rules are an autonomous controller's answers: what Drive says to each
// question a run asks, in place of a person.
type Rules struct {
	// Permit decides a call the policy asked about: allow, or refuse,
	// with a note the model is told either way, whether a refusal ends
	// the run, who decided, and how far an approval reaches (a grant
	// for the session or kept for the project, of the rule the session
	// offers, Session.GrantOffer). reason is the policy's question. Nil
	// refuses every call.
	Permit func(call *openresponses.FunctionCall, reason string) PermitAnswer
	// Reply answers a question asked while a call runs: a sub-agent's
	// call the policy asks about (q.Call), a tool's own question. What
	// it says with the answer is its Note, which the model is told of an
	// answer about a call. Nil leaves the questions unasked: a
	// sub-agent's call stays deferred, its task call failing
	// input-required, and a tool's question is cancelled.
	Reply func(q agentturn.Question) agenttool.Answer
	// Refused, when set, is told of each call Permit refused.
	Refused func(call *openresponses.FunctionCall)
	// By, when set, names who gave the reply Reply has just given to q,
	// written beside the answer (agentturn.ContextWithAnswerer):
	// AtTerminal for a person's answer, a trigger of the controller's
	// own for a rule's. It is asked per reply, so rules that ask a
	// person and answer for them when nobody can (a decline at the end
	// of the input) name the person only for what the person said. The
	// zero Trigger, or a nil By, leaves the reply unattributed.
	By func(q agentturn.Question) agentturn.Trigger
}

// PermitAnswer is a controller's answer to a call the policy asked
// about.
type PermitAnswer struct {
	// Allow runs the call; Note is what the model is told with the
	// answer, either way.
	Allow bool
	Note  string
	// Stop, for a refusal, ends the run (Refusal with stop) rather than
	// going on to the model.
	Stop bool
	// By names who decided, the decider the record writes:
	// agentpolicy.ByHuman ("") for a person's answer, ByPolicy for the
	// controller's own rule, such as a refusal because nobody was left
	// to ask (the end of -p's input, its -timeout). A rule's refusal
	// tells the model nobody answered, not that the user denied the
	// call.
	By string
	// Grant, for an approval, is how far it reaches, and Rule the rule
	// the session offered for the call (Session.GrantOffer). Drive makes
	// the grant through the control when it is a Session, before the
	// Resume that answers the call, and only for a person's answer (By
	// ByHuman): a rule's approval is once.
	Grant GrantKind
	Rule  string
}

// Drive is the human or autonomous plane as a controller that answers
// by rule: it prompts c with items and runs until the agent is idle,
// answering every permission with r.Permit and every question with
// r.Reply, and returns how the last run ended. It holds nothing but
// agentturn's control contract, so it drives a session in this process
// or one behind a wire alike. Calls a stopped session held for
// approval are put to r.Permit first, and items go in as a steer of the
// run that answers them.
func Drive(ctx context.Context, c agentturn.Control, r Rules, items ...openresponses.Item) (*agentturn.RunEnd, error) {
	if r.Reply != nil {
		if a, ok := c.(interface{ Answering() func() }); ok {
			defer a.Answering()()
		}
		defer c.Subscribe(func(_ context.Context, ev agentturn.Event) error {
			if q, ok := ev.(*agentturn.Question); ok {
				q := *q
				// The rules may wait on a person: they answer off the
				// event's delivery, so an abort is not held up behind
				// them.
				go func() {
					ans := r.Reply(q)
					// A reply goes under Drive's context, naming who
					// gave it, not the event's, which names who
					// started the run.
					rctx := ctx
					if r.By != nil {
						if by := r.By(q); by.Kind != "" || by.Ref != "" || by.Source != "" {
							rctx = agentturn.ContextWithAnswerer(ctx, by)
						}
					}
					c.Reply(rctx, q.ID, ans)
				}()
			}
			return nil
		})()
	}
	permit := func(call *openresponses.FunctionCall, reason string) agentturn.Answer {
		var pa PermitAnswer
		if r.Permit != nil {
			pa = r.Permit(call, reason)
		}
		note, by := pa.Note, pa.By
		if by == "" {
			by = agentpolicy.ByHuman
		}
		if pa.Allow {
			// A grant is a person's to make: a rule's approval is once.
			if pa.Grant != GrantOnce && by != agentpolicy.ByHuman {
				fmt.Fprintf(warnings{}, "dax: %s approved once: a grant is a person's, and %s decided\n", call.Name, by)
			} else if pa.Grant != GrantOnce {
				g, ok := c.(interface {
					Grant(context.Context, string, GrantKind, string) error
				})
				switch {
				case !ok:
					fmt.Fprintf(warnings{}, "dax: %s approved once: this session cannot grant %s\n", call.Name, pa.Rule)
				default:
					if err := g.Grant(ctx, call.CallID, pa.Grant, pa.Rule); err != nil {
						fmt.Fprintf(warnings{}, "dax: %s approved once: %v\n", call.Name, err)
					}
				}
			}
			a := agentturn.Approve(call.CallID).WithBy(by)
			if note != "" {
				a = a.WithNote(note)
			}
			return a
		}
		if r.Refused != nil {
			r.Refused(call)
		}
		stop := pa.Stop
		if by != agentpolicy.ByHuman {
			o := openresponses.NewFunctionCallOutput(call.CallID, unansweredOutput)
			if stop {
				return agentturn.Refuse(o).WithBy(by)
			}
			return agentturn.Output(o).WithBy(by)
		}
		return Refusal(call.CallID, note, stop)
	}
	// Drive's items are the run's task: a fold keeps them verbatim,
	// though they claim no person.
	pinTasks(c, items)
	var end *agentturn.RunEnd
	var err error
	if pending := c.State().Pending; slices.ContainsFunc(pending, held) {
		// Only the calls that wait on a person are put to r.Permit; the
		// ones held beside them are released by the answers.
		var answers []agentturn.Answer
		for _, p := range HeldPermissions(c) {
			answers = append(answers, permit(p.Call, p.Reason))
		}
		if len(items) > 0 {
			if err := c.Queue(ctx, agentturn.QueueSteer, items...); err != nil {
				return nil, err
			}
		}
		end, err = c.Resume(ctx, answers...)
	} else {
		end, err = c.Prompt(ctx, items...)
	}
	for err == nil && end.Reason == agentturn.ReasonInputRequired {
		var answers []agentturn.Answer
		for _, p := range PermissionsOf(c, end) {
			answers = append(answers, permit(p.Call, p.Reason))
		}
		end, err = c.Resume(ctx, answers...)
	}
	return end, err
}

// Refusal is a person's refusal of a call, the model told so, told not
// to get the same result another way, and, when note is not empty, why.
// With stop the refusal ends the run (agentturn.Refuse) rather than
// going on to the model; the words are the same either way, so every
// front that refuses through it tells the model the same thing.
func Refusal(callID, note string, stop bool) agentturn.Answer {
	out := deniedOutput
	if note != "" {
		out += " Reason: " + note
	}
	o := openresponses.NewFunctionCallOutput(callID, out)
	a := agentturn.Output(o)
	if stop {
		a = agentturn.Refuse(o)
	}
	a = a.WithBy(agentpolicy.ByHuman)
	if note != "" {
		// The person's reason is the answer's too, as the record
		// keeps it.
		a = a.WithReason(note)
	}
	return a
}

// HeldPermissions are the calls a stopped session left held for
// approval that wait on a person, as PermissionsOf reads them from a
// run's end: after a restart, each with the question and the part of
// the call the restored decision is about, and not the calls held only
// beside another's question, which Resume releases. A call whose
// decision did not survive is asked about as held when the session
// stopped.
func HeldPermissions(c agentturn.Control) []Permission {
	st := c.State()
	end := &agentturn.RunEnd{RunID: st.RunID, Reason: agentturn.ReasonInputRequired}
	for _, p := range st.Pending {
		if held(p) {
			end.Pending = append(end.Pending, p)
		}
	}
	perms := PermissionsOf(c, end)
	for i := range perms {
		if perms[i].Reason == "" {
			perms[i].Reason = heldReason
		}
	}
	return perms
}

// heldReason is the question about a held call whose decision says
// nothing of why.
const heldReason = "held for approval when the session stopped"

// PermissionsOf are the calls a run that ended for input left for a
// person: the Session's own Permissions when c is one, and otherwise
// (a control behind a wire) the pending calls whose decision was not
// only held beside another's question, with the decision's question and
// the part of the call it is about.
func PermissionsOf(c agentturn.Control, end *agentturn.RunEnd) []Permission {
	if p, ok := c.(interface {
		Permissions(*agentturn.RunEnd) []Permission
	}); ok {
		return p.Permissions(end)
	}
	if end == nil || end.Reason != agentturn.ReasonInputRequired {
		return nil
	}
	var out []Permission
	for _, p := range end.Pending {
		if p.Call == nil {
			continue
		}
		perm := Permission{Call: p.Call}
		if d := p.Decision; d != nil {
			if d.Held {
				continue
			}
			perm.Reason = about(d.Reason, d.Subject)
		}
		out = append(out, perm)
	}
	return out
}

// unstamped is args without dax's stamp (dax_stamp), the rewrite a
// file tool's or bash's facts claim adds for the call to run with; args
// as they are when they hold none or are not an object.
func unstamped(args json.RawMessage) json.RawMessage { return Unstamped(args) }

// Unstamped is a call's arguments as the model made them: without the
// stamp a tool's facts claim added for the run (dax's, not the call's),
// for a front that shows the call (ACP's tool_call).
func Unstamped(args json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(args, &m) != nil {
		return args
	}
	if _, ok := m["dax_stamp"]; !ok {
		return args
	}
	delete(m, "dax_stamp")
	out, err := json.Marshal(m)
	if err != nil {
		return args
	}
	return out
}
