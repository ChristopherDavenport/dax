package agents

// Background sub-agents: explore and task take an optional
// `background: true`, which returns the call at once with an id and
// delivers the child's answer to the session when it ends, as the
// agent's own work (extension.Env.Deliver): it starts a run when the
// session is idle, and joins the run in flight when that run will still
// take it. background_agents lists the backgrounded children and
// cancels one.
//
// agentturn's WithDetach is a choice of the tool, not of the call, so
// each of explore and task is two child tools: the foreground one the
// model has always had, and a background one, registered as
// <name>_background with WithDetach. The model is offered one, explore
// or task, with the extra argument (Env.WrapOwned): background false
// runs the foreground tool, true the background one, both under the one
// name, so the policy decides, the record shows and the renderers draw
// the call as explore's or task's.
//
// What holds for a background child:
//   - at most maxBackground run at once; a start past it is refused, as
//     a ninth background process is, so a model cannot fan out without
//     bound;
//   - it ends with the session: the session's close aborts it
//     (Env.OnClose) before the record closes, and nothing about it is
//     kept for a restart;
//   - a user's abort of the parent's run cuts it too (agentturn gives
//     it the call's run context), and so does background_agents cancel;
//     neither is reported back, since the model that cancelled knows and
//     the user that aborted does not want a run started;
//   - it cannot ask: under WithAskParent a call the policy asks about is
//     put to whoever answers the task call's questions, and there is no
//     call to answer for a backgrounded child, so the child stops with
//     its call pending and its end is delivered as a failure that names
//     the call. It is never left to wait, and a foreground run of the
//     same task is the way to approve it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	childagent "github.com/ChristopherDavenport/agentturn/tools/agent"

	"github.com/ChristopherDavenport/dax/extension"
)

const (
	// maxBackground is the most background sub-agents that run at once.
	maxBackground = 4
	// keepEnded is how many ended background sub-agents the list keeps.
	keepEnded = 16
	// ControlTool is the tool that lists and cancels the background
	// sub-agents.
	ControlTool = "background_agents"
	// answerBytes is the most of a background child's answer delivered.
	answerBytes = 16 << 10
	// closeWait is how long the session's close waits for the children
	// it aborted to end.
	closeWait = 5 * time.Second
)

// backgroundArg is the property added to the tools' arguments.
const backgroundArg = "background"

const backgroundDesc = "true: return at once with an id and go on; the sub-agent's answer is delivered to you as a message when it ends, so do not wait for it or poll. " +
	"At most 4 run at once; background_agents lists and cancels them. A background sub-agent cannot ask about a call the policy asks about: it stops and says so. It ends with the session."

// backgrounds is one session's background sub-agents.
type backgrounds struct {
	env extension.Env

	mu    sync.Mutex
	twins map[string]agenttool.Tool // the background tools, by the name the model calls
	jobs  []*job
	next  int
	wg    sync.WaitGroup // the jobs not yet ended
}

// job is one background sub-agent.
type job struct {
	id      string
	name    string // explore or task
	call    string
	task    string
	started time.Time
	child   *agentturn.Agent

	state    string // running, finished, failed, needs input, cancelled, aborted
	ended    time.Time
	cancel   bool
	closing  bool
	finished bool // the detach function has run
}

func newBackgrounds(env extension.Env) *backgrounds {
	b := &backgrounds{env: env, twins: map[string]agenttool.Tool{}}
	env.OnClose(b.stop)
	return b
}

// options are the child options that make a tool a background one: the
// child's agent, for cancel, and its end, for delivery.
func (b *backgrounds) options() []childagent.Option {
	return []childagent.Option{
		childagent.WithSpawn(func(callID string, child *agentturn.Agent) {
			b.mu.Lock()
			defer b.mu.Unlock()
			if j := b.byCall(callID); j != nil {
				j.child = child
			}
		}),
		childagent.WithDetach(b.ended),
	}
}

// byCall is the job a call started, under mu.
func (b *backgrounds) byCall(callID string) *job {
	for _, j := range b.jobs {
		if j.call == callID {
			return j
		}
	}
	return nil
}

// running is the number of jobs that run, under mu.
func (b *backgrounds) running() int {
	n := 0
	for _, j := range b.jobs {
		if j.state == "running" {
			n++
		}
	}
	return n
}

// reserve makes the job a call is about to start, or says why not.
func (b *backgrounds) reserve(name, callID, task string) (*job, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n := b.running(); n >= maxBackground {
		return nil, fmt.Errorf("%d background sub-agents are running, the most there are; wait for one's answer, or cancel one with %s", n, ControlTool)
	}
	b.next++
	j := &job{id: fmt.Sprintf("%s-%d", name, b.next), name: name, call: callID, task: task, started: time.Now(), state: "running"}
	b.jobs = append(b.jobs, j)
	b.wg.Add(1)
	return j, nil
}

// release forgets a job whose call did not start it.
func (b *backgrounds) release(j *job) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.jobs = slices.DeleteFunc(b.jobs, func(x *job) bool { return x == j })
	if !j.finished {
		j.finished = true
		b.wg.Done()
	}
}

// ended is the detach function: the child's run ended, on its goroutine.
func (b *backgrounds) ended(callID string, end *agentturn.RunEnd) {
	b.mu.Lock()
	j := b.byCall(callID)
	if j == nil || j.finished {
		b.mu.Unlock()
		return
	}
	j.finished, j.ended, j.child = true, time.Now(), nil
	quiet := j.cancel || j.closing || end.Reason == agentturn.ReasonAborted
	switch {
	case j.cancel || j.closing:
		j.state = "cancelled"
	case end.Reason == agentturn.ReasonAborted:
		j.state = "aborted"
	case end.Reason == agentturn.ReasonInputRequired:
		j.state = "needs input"
	case end.Reason == agentturn.ReasonError:
		j.state = "failed"
	default:
		j.state = "finished"
	}
	d := j.delivery(end)
	b.trim()
	b.mu.Unlock()
	defer b.wg.Done()
	if quiet {
		return
	}
	if err := b.env.Deliver(d); err != nil {
		b.env.Log("background sub-agent %s ended, but its result was not delivered: %v", j.id, err)
	}
}

// trim drops the oldest ended jobs past keepEnded, under mu.
func (b *backgrounds) trim() {
	ended := 0
	for _, j := range b.jobs {
		if j.finished {
			ended++
		}
	}
	for ended > keepEnded {
		i := slices.IndexFunc(b.jobs, func(j *job) bool { return j.finished })
		b.jobs = slices.Delete(b.jobs, i, i+1)
		ended--
	}
}

// delivery is the job's end as the session is told of it. The header is
// dax's words; whatever the child wrote or an error says is the payload,
// which the session frames as untrusted.
func (j *job) delivery(end *agentturn.RunEnd) extension.Delivery {
	took := j.ended.Sub(j.started).Round(time.Second)
	var how, payload string
	switch end.Reason {
	case agentturn.ReasonError:
		how, payload = "failed", fmt.Sprint(end.Err)
	case agentturn.ReasonAborted:
		how, payload = "was cut off", "[no answer]"
	case agentturn.ReasonInputRequired:
		how = "stopped: it needs approval for a call, and a background sub-agent cannot ask; run the task in the foreground, or allow the call"
		var calls []string
		for _, p := range end.Pending {
			if p.Call != nil {
				calls = append(calls, p.Call.Name+" "+p.Call.Arguments)
			}
		}
		payload = "[waiting on: " + strings.Join(calls, "; ") + "]"
	default:
		how = "finished"
		text, ok := end.Answer()
		switch {
		case ok:
			payload = text
		case end.Reason == agentturn.ReasonStopped:
			how, payload = fmt.Sprintf("stopped without an answer (%s)", end.Cause), "[no answer]"
		default:
			how, payload = "ended without an answer", "[no answer]"
		}
	}
	return extension.Delivery{
		Trigger:  agentturn.CalleeTrigger(j.call, clip(j.task, 200)),
		Header:   fmt.Sprintf("[Background %s sub-agent (id %s, call %s) %s after %s.]", j.name, j.id, j.call, how, took),
		Payload:  payload,
		MaxBytes: answerBytes,
	}
}

// stop aborts the running jobs and waits for them to end, at the
// session's close.
func (b *backgrounds) stop() {
	b.mu.Lock()
	for _, j := range b.jobs {
		if j.state == "running" {
			j.closing = true
			if j.child != nil {
				j.child.Abort()
			}
		}
	}
	b.mu.Unlock()
	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(closeWait):
	}
}

// dispatch is the tool the model is offered under a foreground tool's
// name: the foreground tool, and the background one when the call says
// background: true.
type dispatch struct {
	fg agenttool.Tool
	b  *backgrounds
}

func (d *dispatch) Name() string        { return d.fg.Name() }
func (d *dispatch) Description() string { return d.fg.Description() }
func (d *dispatch) Strict() bool        { return agenttool.IsStrict(d.fg) }

// Parameters are the foreground tool's, and the background property.
func (d *dispatch) Parameters() json.RawMessage {
	var schema map[string]json.RawMessage
	if json.Unmarshal(d.fg.Parameters(), &schema) != nil {
		return d.fg.Parameters()
	}
	var props map[string]json.RawMessage
	if json.Unmarshal(schema["properties"], &props) != nil {
		return d.fg.Parameters()
	}
	prop, _ := json.Marshal(map[string]any{"type": "boolean", "description": backgroundDesc})
	props[backgroundArg] = prop
	schema["properties"], _ = json.Marshal(props)
	out, err := json.Marshal(schema)
	if err != nil {
		return d.fg.Parameters()
	}
	return out
}

// Replay is the foreground tool's for a call that is not backgrounded,
// which a run cut off replays under its key; a backgrounded call's
// child is not continued, as agentturn says of a detached call.
func (d *dispatch) Replay(ctx context.Context, args json.RawMessage) agenttool.Replay {
	if bg, _ := splitBackground(args); bg {
		return agenttool.ReplayUnknown
	}
	return agenttool.ReplayOf(ctx, d.fg, args)
}

func (d *dispatch) Annotations() agenttool.Annotations { return agenttool.AnnotationsOf(d.fg) }

// Execute runs the call as a foreground child, or starts it in the
// background.
func (d *dispatch) Execute(ctx context.Context, call agenttool.Call) (agenttool.Result, error) {
	bg, rest := splitBackground(call.Args)
	call.Args = rest
	if !bg {
		return d.fg.Execute(ctx, call)
	}
	d.b.mu.Lock()
	twin := d.b.twins[d.Name()]
	d.b.mu.Unlock()
	if twin == nil {
		return agenttool.Result{}, errors.New("background is not available for this sub-agent")
	}
	var in struct {
		Input string `json:"input"`
	}
	_ = json.Unmarshal(rest, &in)
	j, err := d.b.reserve(d.Name(), call.ID, in.Input)
	if err != nil {
		return agenttool.Result{}, err
	}
	res, err := twin.Execute(ctx, call)
	if err != nil {
		d.b.release(j)
		return res, err
	}
	res.Output.Text += fmt.Sprintf("\nBackground id %s: its answer is delivered to you as a message when it ends; do not wait for it. "+
		"%s lists and cancels background sub-agents. It ends with the session.", j.id, ControlTool)
	return res, nil
}

// splitBackground reads the background argument and returns the
// arguments without it, which the child tools' decoders take.
func splitBackground(args json.RawMessage) (bool, json.RawMessage) {
	var m map[string]json.RawMessage
	if json.Unmarshal(args, &m) != nil {
		return false, args
	}
	raw, ok := m[backgroundArg]
	if !ok {
		return false, args
	}
	var bg bool
	_ = json.Unmarshal(raw, &bg)
	delete(m, backgroundArg)
	out, err := json.Marshal(m)
	if err != nil {
		return false, args
	}
	return bg, out
}

// wrap is the extension's WrapOwned function: the foreground tools
// become dispatchers, the background ones are kept and left out of what
// the model is offered.
func (b *backgrounds) wrap(t agenttool.Tool) agenttool.Tool {
	name := t.Name()
	if fg, ok := strings.CutSuffix(name, "_background"); ok {
		b.mu.Lock()
		b.twins[fg] = t
		b.mu.Unlock()
		return nil
	}
	if name == ControlTool {
		return t
	}
	return &dispatch{fg: t, b: b}
}

// ControlArgs are background_agents' arguments.
type ControlArgs struct {
	Action string `json:"action" enum:"list,cancel" desc:"list the background sub-agents, or cancel one"`
	ID     string `json:"id,omitempty" desc:"For cancel: the sub-agent's id, as the call that started it and list give it"`
}

// control is the background_agents tool.
func (b *backgrounds) control() agenttool.Tool {
	return agenttool.New(ControlTool, "List the sub-agents you started with background: true, and cancel one. "+
		"A cancelled sub-agent reports nothing further. They end with the session, so after a resume the list is empty.",
		func(_ context.Context, in ControlArgs) (string, error) {
			switch in.Action {
			case "list":
				return b.list(), nil
			case "cancel":
				return b.cancelJob(in.ID)
			}
			return "", fmt.Errorf("action %q: want list or cancel", in.Action)
		})
}

func (b *backgrounds) list() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.jobs) == 0 {
		return "no background sub-agents"
	}
	var s strings.Builder
	for _, j := range b.jobs {
		state := j.state
		if j.state == "running" {
			state += " for " + time.Since(j.started).Round(time.Second).String()
		}
		fmt.Fprintf(&s, "[%s] %s: %s\n", j.id, state, oneLine(j.task))
	}
	return strings.TrimRight(s.String(), "\n")
}

func (b *backgrounds) cancelJob(id string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, j := range b.jobs {
		if j.id != id {
			continue
		}
		if j.state != "running" {
			return fmt.Sprintf("[%s] is not running (%s)", j.id, j.state), nil
		}
		j.cancel = true
		if j.child != nil {
			j.child.Abort()
		}
		return fmt.Sprintf("[%s] cancelled; it reports nothing further", j.id), nil
	}
	return "", fmt.Errorf("no background sub-agent %q; list them with action list", id)
}

// oneLine is s as a line of at most 100 bytes.
func oneLine(s string) string { return clip(strings.Join(strings.Fields(s), " "), 100) }

// clip is s cut to at most n bytes at a character.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}
