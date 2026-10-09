package agent

// This file is the session's human or autonomous plane: the Turn a
// front, a controller or (later) a wire drives, dax's own controls
// beside it, and Drive, a controller that answers by rule.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/internal/render"
)

// ErrHeld is what Prompt returns while calls a stopped session held for
// approval wait for an answer: nobody has said whether they may run, so
// they are answered first, with Answer, and the prompt goes in as a
// steer of that run (Drive does both).
var ErrHeld = errors.New("calls held for approval when the session stopped wait for an answer")

// Turn is a session's agent as its human or autonomous plane drives it,
// in agentturn's own terms: prompt, steer, answer, abort, and the
// events of each run. A person at a front and a controller that answers
// by rule (Drive) hold the same Turn; a wire to a remote front would
// carry it. It is the shape dax proposes for agentturn, beside its A2A
// and Responses fronts, and the Session is its in-process
// implementation: the case with the wire left out.
//
// It differs from agentturn.Agent where a human plane needs more than
// the loop: Answer puts the answers through the policy engine, which
// releases the calls it only held beside the ones it asked about;
// Prompt and Answer answer for the calls an abort or a restart left
// unanswered; and questions asked while a call runs (a sub-agent's call
// the policy asks about, a tool's own question) come to the Turn's
// holder, not to a hook the session was built with.
type Turn interface {
	// Prompt appends items and runs until the agent is idle. The calls
	// an abort or a restart left unanswered are answered ahead of the
	// items with what is known of them (cut off and may have run; never
	// reached its tool), in the same model call. While calls a stopped
	// session held for approval wait, it returns ErrHeld.
	Prompt(ctx context.Context, items ...openresponses.Item) (*agentturn.RunEnd, error)
	// Answer answers the calls a run left pending and runs on: the
	// reply to a permission. It is agentturn.Agent's Resume with the
	// policy engine between: a call the engine only held is released
	// with the batch, and a call that waits on no one is answered with
	// what is known of it. It is named for what the human plane does,
	// answering, where Resume names what the loop does.
	Answer(ctx context.Context, answers ...agentturn.Answer) (*agentturn.RunEnd, error)
	// Permissions are the calls a run that ended for input left for a
	// person, each with the policy's question: not the calls the engine
	// holds beside them, which Answer releases.
	Permissions(end *agentturn.RunEnd) []Permission
	// Steer queues items into the run in flight, after its tool batch,
	// or into the next run; FollowUp queues them for when a run would
	// otherwise end. Both are written to the session as queued entries
	// before they return.
	Steer(ctx context.Context, items ...openresponses.Item) error
	FollowUp(ctx context.Context, items ...openresponses.Item) error
	// Abort cancels the run in flight.
	Abort()
	// State is a snapshot: the transcript, the pending calls, whether a
	// run goes.
	State() agentturn.State
	// Subscribe delivers the agent's events, in order, on the loop's
	// goroutine, as agentturn.Agent.Subscribe does.
	Subscribe(fn func(context.Context, agentturn.Event) error) (unsubscribe func())
	// Questions delivers each question asked while a call runs, and the
	// ones already waiting when it is called. fn must return at once;
	// Reply answers. With no one subscribed a question is not asked: a
	// sub-agent's call is refused with a note to make it from the main
	// agent, and a tool's question is cancelled.
	Questions(fn func(Question)) (unsubscribe func())
	// Reply answers a question. A question no longer waiting is an
	// error.
	Reply(id string, r Reply) error
}

// Permission is a call a run left for a person to answer.
type Permission struct {
	Call *openresponses.FunctionCall
	// Reason is the policy's question, with what it was asking about:
	// the rule, the secret path, the git config key, the part of a
	// command line.
	Reason string
}

// Question is asked while a call runs, and the call waits for the
// reply.
type Question struct {
	ID string
	// Call is the call it is about, a sub-agent's; nil for a tool's own
	// question.
	Call *openresponses.FunctionCall
	Text string
	// Done is closed when the question no longer waits: answered, or
	// given up by the call that asked it (an abort).
	Done <-chan struct{}
}

// Reply answers a Question: Accept allows the call or says yes; Note is
// what the person typed with it, a refusal's reason.
type Reply struct {
	Accept bool
	Note   string
}

var _ Turn = (*Session)(nil)

// Turn is the session's Turn.
func (s *Session) Turn() Turn { return s }

// Agent is the session's one agent, for a view that needs it: the
// terminal client's backend (agentconsole's native) takes the agent to
// follow. Drive the session through its Turn.
func (s *Session) Agent() *agentturn.Agent { return s.ag }

// Record is the store the session is written to, as a view follows it
// (Follow from Info's ID), or nil for a store that cannot be followed.
// A session nothing is kept of is written to memory and followed the
// same way.
func (s *Session) Record() agentsession.Follower {
	f, _ := s.store.(agentsession.Follower)
	return f
}

// RunContext puts on ctx what the kit expects of a run it records: its
// recorder, the session's ID for the model calls, and the session for
// memory a run writes. Prompt and Answer use it; a view that starts a
// run of its own (the terminal client's head move) does too.
func (s *Session) RunContext(ctx context.Context) context.Context {
	rec := s.Kit.Recorder()
	if rec == nil {
		return ctx
	}
	ctx = agentkit.ContextWithRecorder(ctx, rec)
	ctx = session.ContextWithSessionID(ctx, rec.SessionID())
	return agentmemory.WithSession(ctx, rec.SessionID())
}

// ended keeps how the last run ended, which Answer gives the engine.
func (s *Session) ended(end *agentturn.RunEnd) {
	if end == nil {
		return
	}
	s.mu.Lock()
	s.lastEnd = end
	s.mu.Unlock()
}

// Prompt implements Turn.
func (s *Session) Prompt(ctx context.Context, items ...openresponses.Item) (*agentturn.RunEnd, error) {
	pending := s.ag.State().Pending
	if slices.ContainsFunc(pending, held) {
		return nil, ErrHeld
	}
	outs := make([]openresponses.Item, 0, len(pending)+len(items))
	for _, p := range pending {
		outs = append(outs, openresponses.NewFunctionCallOutput(p.Call.CallID, output(p)))
	}
	end, err := s.ag.Prompt(s.RunContext(ctx), append(outs, items...)...)
	s.ended(end)
	return end, err
}

// Answer implements Turn.
func (s *Session) Answer(ctx context.Context, answers ...agentturn.Answer) (*agentturn.RunEnd, error) {
	ctx = s.RunContext(ctx)
	given := map[string]bool{}
	for _, a := range answers {
		given[a.CallID] = true
	}
	for _, p := range s.ag.State().Pending {
		if !held(p) && !given[p.Call.CallID] {
			answers = append(answers, agentturn.Output(openresponses.NewFunctionCallOutput(p.Call.CallID, output(p))))
		}
	}
	if eng := s.Kit.Engine(); eng != nil {
		s.mu.Lock()
		end := s.lastEnd
		s.mu.Unlock()
		// An end none of the answers is about is another run's.
		if end != nil && !slices.ContainsFunc(answers, func(a agentturn.Answer) bool {
			return slices.ContainsFunc(end.Pending, func(p agentturn.PendingCall) bool { return p.Call != nil && p.Call.CallID == a.CallID })
		}) {
			end = nil
		}
		var err error
		if answers, err = eng.Release(ctx, end, answers...); err != nil {
			return nil, err
		}
	}
	end, err := s.ag.Resume(ctx, answers...)
	s.ended(end)
	return end, err
}

// Permissions implements Turn: the pending calls the policy asked about,
// with its question, and not the ones its engine only held.
func (s *Session) Permissions(end *agentturn.RunEnd) []Permission {
	if end == nil || end.Reason != agentturn.ReasonInputRequired {
		return nil
	}
	eng := s.Kit.Engine()
	var out []Permission
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
		out = append(out, Permission{Call: p.Call, Reason: reason})
	}
	return out
}

// Steer implements Turn.
func (s *Session) Steer(ctx context.Context, items ...openresponses.Item) error {
	return s.queue(ctx, agentturn.QueueSteer, items)
}

// FollowUp implements Turn.
func (s *Session) FollowUp(ctx context.Context, items ...openresponses.Item) error {
	return s.queue(ctx, agentturn.QueueFollowUp, items)
}

// queue writes each item as a queued entry before the agent takes it,
// so a steer is on the record before a quiet tool's next event would
// write it.
func (s *Session) queue(ctx context.Context, mode agentturn.QueueMode, items []openresponses.Item) error {
	if rec := s.Kit.Recorder(); rec != nil {
		return rec.Queue(ctx, s.ag, mode, items...)
	}
	return s.ag.Queue(ctx, mode, items...)
}

// Abort implements Turn.
func (s *Session) Abort() { s.ag.Abort() }

// State implements Turn.
func (s *Session) State() agentturn.State { return s.ag.State() }

// Subscribe implements Turn.
func (s *Session) Subscribe(fn func(context.Context, agentturn.Event) error) func() {
	return s.ag.Subscribe(fn)
}

// Questions implements Turn.
func (s *Session) Questions(fn func(Question)) func() { return s.asks.subscribe(fn) }

// Reply implements Turn.
func (s *Session) Reply(id string, r Reply) error {
	if !s.asks.close(id, &r) {
		return fmt.Errorf("no question %q is waiting", id)
	}
	return nil
}

// errNoOne is a question asked with nobody holding the Turn's questions.
var errNoOne = errors.New("nobody is answering questions")

// asks are the questions waiting and the subscribers to tell of them.
type asks struct {
	mu      sync.Mutex
	seq     int
	waiting []*ask
	subs    map[int]func(Question)
	nsub    int
}

type ask struct {
	q     Question
	done  chan struct{}
	reply chan Reply
}

// put asks a question and waits for the reply; with nobody subscribed
// it returns errNoOne at once, and when ctx ends first (an abort) it
// gives the question up and returns ctx's error.
func (h *asks) put(ctx context.Context, call *openresponses.FunctionCall, text string) (Reply, error) {
	h.mu.Lock()
	if len(h.subs) == 0 {
		h.mu.Unlock()
		return Reply{}, errNoOne
	}
	h.seq++
	a := &ask{done: make(chan struct{}), reply: make(chan Reply, 1)}
	a.q = Question{ID: fmt.Sprintf("q%d", h.seq), Call: call, Text: text, Done: a.done}
	h.waiting = append(h.waiting, a)
	subs := make([]func(Question), 0, len(h.subs))
	for _, fn := range h.subs {
		subs = append(subs, fn)
	}
	h.mu.Unlock()
	for _, fn := range subs {
		fn(a.q)
	}
	select {
	case r := <-a.reply:
		return r, nil
	case <-ctx.Done():
		h.close(a.q.ID, nil)
		return Reply{}, ctx.Err()
	}
}

// close ends the question id with r, nil when it was given up, and
// reports whether it was waiting.
func (h *asks) close(id string, r *Reply) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	i := slices.IndexFunc(h.waiting, func(a *ask) bool { return a.q.ID == id })
	if i < 0 {
		return false
	}
	a := h.waiting[i]
	h.waiting = slices.Delete(h.waiting, i, i+1)
	if r != nil {
		a.reply <- *r
	}
	close(a.done)
	return true
}

func (h *asks) subscribe(fn func(Question)) func() {
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[int]func(Question){}
	}
	h.nsub++
	id := h.nsub
	h.subs[id] = fn
	waiting := make([]Question, 0, len(h.waiting))
	for _, a := range h.waiting {
		waiting = append(waiting, a.q)
	}
	h.mu.Unlock()
	for _, q := range waiting {
		fn(q)
	}
	return func() {
		h.mu.Lock()
		delete(h.subs, id)
		h.mu.Unlock()
	}
}

// elicit puts a tool's yes-or-no question to whoever holds the Turn's
// questions. A form or a page to visit has no place in a Question, so
// it is cancelled, and noted.
func (s *Session) elicit(ctx context.Context, q agenttool.Elicitation) (agenttool.Answer, error) {
	switch {
	case q.URL != "":
		s.opts.log("[a tool asks you to visit %s: %s]", render.Clean(q.URL), render.Clean(q.Message))
		return agenttool.Answer{Action: agenttool.ActionCancel}, nil
	case hasFields(q.Schema):
		s.opts.log("[a tool asks for a form dax cannot show: %s]", render.Clean(q.Message))
		return agenttool.Answer{Action: agenttool.ActionCancel}, nil
	}
	r, err := s.asks.put(ctx, nil, "a tool asks: "+q.Message)
	switch {
	case err != nil:
		return agenttool.Answer{Action: agenttool.ActionCancel}, nil
	case r.Accept:
		return agenttool.Answer{Action: agenttool.ActionAccept, Content: json.RawMessage(`{}`)}, nil
	}
	return agenttool.Answer{Action: agenttool.ActionDecline}, nil
}

// hasFields reports whether a form's schema asks for any property.
func hasFields(schema json.RawMessage) bool {
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	return len(schema) > 0 && json.Unmarshal(schema, &s) == nil && len(s.Properties) > 0
}

// Controls are dax's own controls of a session, beside its Turn: what a
// front's slash commands do and what its start lines show. They are a
// product's, not every agent's, so they are not part of the Turn: the
// model and the reasoning setting, the MCP servers, and the session's
// assembly. A wire to a remote front carries them beside the Turn; the
// Session is their in-process implementation.
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
	// it was assembled with and what its instruction layers left out.
	Info() Info
}

// Info describes a session for a front's start lines.
type Info struct {
	// ID is the session's ID. Recorded is false for a session nothing
	// is kept of (no store), whose ID names nothing after it closes.
	ID       string
	Recorded bool
	// Path is its directory in a local store; empty otherwise.
	Path    string
	Tools   []agentkit.ToolOrigin
	Omitted []agentkit.Omission
}

var _ Controls = (*Session)(nil)

// Info implements Controls.
func (s *Session) Info() Info {
	return Info{ID: s.ID(), Recorded: s.recorded, Path: s.Path(), Tools: s.Tools(), Omitted: s.Omitted()}
}

// Rules are an autonomous controller's answers: what Drive says to each
// question a run asks, in place of a person.
type Rules struct {
	// Permit decides a call the policy asked about: allow, or refuse
	// with a note the model is told. reason is the policy's question.
	// Nil refuses every call.
	Permit func(call *openresponses.FunctionCall, reason string) (allow bool, note string)
	// Reply answers a question asked while a call runs: a sub-agent's
	// call the policy asks about, a tool's own question. Nil leaves the
	// questions unasked: a sub-agent's call is refused with a note to
	// make it from the main agent, and a tool's question is cancelled.
	Reply func(q Question) Reply
	// Refused, when set, is told of each call Permit refused.
	Refused func(call *openresponses.FunctionCall)
}

// Drive is the human or autonomous plane as a controller that answers
// by rule: it prompts t with items and runs until the agent is idle,
// answering every permission with r.Permit and every question with
// r.Reply, and returns how the last run ended. It holds nothing but the
// Turn, so it drives a session in this process or one behind a wire
// alike. Calls a stopped session held for approval are put to r.Permit
// first, and items go in as a steer of the run that answers them.
func Drive(ctx context.Context, t Turn, r Rules, items ...openresponses.Item) (*agentturn.RunEnd, error) {
	if r.Reply != nil {
		unsub := t.Questions(func(q Question) {
			go t.Reply(q.ID, r.Reply(q))
		})
		defer unsub()
	}
	permit := func(call *openresponses.FunctionCall, reason string) agentturn.Answer {
		allow, note := false, ""
		if r.Permit != nil {
			allow, note = r.Permit(call, reason)
		}
		if allow {
			return agentturn.Approve(call.CallID).WithBy(agentpolicy.ByHuman)
		}
		if r.Refused != nil {
			r.Refused(call)
		}
		out := deniedOutput
		if note != "" {
			out += " Reason: " + note
		}
		return agentturn.Output(openresponses.NewFunctionCallOutput(call.CallID, out)).WithBy(agentpolicy.ByHuman)
	}
	var end *agentturn.RunEnd
	var err error
	if pending := t.State().Pending; slices.ContainsFunc(pending, held) {
		var answers []agentturn.Answer
		for _, p := range pending {
			if held(p) {
				answers = append(answers, permit(p.Call, "held for approval when the session stopped"))
			}
		}
		if len(items) > 0 {
			if err := t.Steer(ctx, items...); err != nil {
				return nil, err
			}
		}
		end, err = t.Answer(ctx, answers...)
	} else {
		end, err = t.Prompt(ctx, items...)
	}
	for err == nil && end.Reason == agentturn.ReasonInputRequired {
		var answers []agentturn.Answer
		for _, p := range t.Permissions(end) {
			answers = append(answers, permit(p.Call, p.Reason))
		}
		end, err = t.Answer(ctx, answers...)
	}
	return end, err
}
