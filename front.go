package dax

// This file is the seam where the front is chosen. A front is the human
// plane of a session: it drives the session as agentturn's control
// contract (prompt, queue, resume with answers, abort, the run's events
// and the questions asked while a call runs, answered with Reply), uses
// dax's own agent.Controls for its slash commands and start lines, and
// may follow the session's record. Everything below a front, the
// policy, the tools, the recording, is the agent package's and does not
// know which front holds it; a front over a wire to a remote session
// would hold the same contract.
//
// There are three: the terminal client (tui.go, agentconsole's console,
// a view of the record and the session's events that sends what the
// user asks to the session as intents), the default when standard input
// and output are a terminal; the REPL, which has the slash commands; and
// print, which is -p and an autonomous controller (agent.Drive) whose
// rules ask on standard input. A new front implements front and gets a
// case in selectFront. The renderer of the agent's events,
// internal/render, is the REPL's and print's.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ChristopherDavenport/agentconsole/client"
	"github.com/ChristopherDavenport/agentconsole/toolview"
	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsmd"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/ext/plan"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/internal/config"
	"github.com/ChristopherDavenport/dax/internal/render"
)

// frontInfo is what a front shows the user about the session.
type frontInfo struct {
	// Name is the program's, which the banner and the resume command
	// name, and Version its version, which dax serve -a2a's card names.
	Name, Version string
	// Renderers draw tool calls in the terminal client: dax's, with
	// the extensions' laid over them.
	Renderers            toolview.Renderers
	Provider, Model, Dir string
	// ModelInfo is a line about what the model takes; empty when its
	// vendor says nothing.
	ModelInfo string
	// Executor names the executor the tools run in and where it acts;
	// empty when they run in this process.
	Executor string
	// Workspace names the served workspace the tools act in (-workspace)
	// and its root; empty when they act in this process's or the
	// executor's.
	Workspace string
	// Servers names the servers workspace beside it (servers_workspace),
	// whose offered servers alone the session connects; empty for none.
	Servers string
	Think   bool
	// Prompt is the one-shot prompt of -p; empty for an interactive
	// front.
	Prompt string
	// Timeout (-timeout) is how long -p's run may take before it is
	// aborted, its cause the timeout; zero is no limit.
	Timeout time.Duration
	// Result (-result) is where -p writes how its run ended, as JSON;
	// with it, a run that ended exits 0 however it ended.
	Result string
	// Trajectory (-trajectory) is where -p writes the session as an
	// ATIF document once its run ends; Price prices it, nil for none.
	Trajectory string
	Price      agent.Price
	// Policy is a line saying what policy is in force.
	Policy string
	// Status is the terminal client's status line from the user's
	// config, nil for the client's default line.
	Status *config.Status
	// UserConfig is the user's config file, which agents_md_budget is
	// raised in; empty when none is read (-bare).
	UserConfig string
	// Windows are the context windows dax believes, by model name, for
	// the status line's context segment.
	Windows map[string]int64
	// Cost prices the terminal client's session pane and status line; nil
	// shows token usage but no cost.
	Cost client.Cost
	// Verbose (-v) has the terminal client print its start lines before
	// it takes the screen and what dax noted after it exits; without it,
	// the client leaves only the command that resumes the session.
	Verbose bool
	// switched is told of the session a front moved to (/new,
	// /resume), which is the one to close at the end; nil for none.
	switched func(*agent.Session)
}

// front is a way to talk to a session.
type front interface {
	// Prepare says where the session's notes go: a front that takes the
	// screen holds them back until it gives it up. It is not how
	// questions reach the user; those are the session's events.
	Prepare(o *agent.Options)
	// Run drives the session until the user is done: the REPL and print
	// through its control contract and Controls alone, the terminal
	// client as agentconsole's source of events and its controller.
	Run(ctx context.Context, sess *agent.Session) error
}

// frontEnv is what selectFront needs to know of the process: whether
// standard input and output are terminals, and whether a session store
// is in use.
type frontEnv struct {
	stdinTTY, stdoutTTY, recording bool
	// trustMCP is -trust-mcp, under which the ACP front starts the
	// editor's MCP servers.
	trustMCP bool
}

// frontEnvOf is processEnv with -trust-mcp.
func frontEnvOf(recording, trustMCP bool) frontEnv {
	e := processEnv(recording)
	e.trustMCP = trustMCP
	return e
}

// processEnv reads it. recording is false when -sessions is empty.
func processEnv(recording bool) frontEnv {
	return frontEnv{stdinTTY: isTerminal(os.Stdin), stdoutTTY: isTerminal(os.Stdout), recording: recording}
}

// selectFront picks the front: print for -p; otherwise the named one,
// and with no name the terminal client when standard input and output
// are terminals and a session is recorded, and the REPL when they are
// not or it is not. The terminal client renders the session's record,
// so it cannot run with recording off, and it needs a terminal; asked
// for by name without either, it fails here, before a store is opened
// or a banner printed.
func selectFront(name, prompt string, info frontInfo, env frontEnv) (front, error) {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	if prompt != "" {
		return &printFront{info: info, in: in}, nil
	}
	if name == "" {
		name = "repl"
		if env.stdinTTY && env.stdoutTTY && env.recording {
			name = "tui"
		}
	}
	switch name {
	case "repl":
		return &replFront{info: info, in: in}, nil
	case "acp":
		// An editor drives the session over standard input and output.
		return &acpFront{info: info, trustMCP: env.trustMCP}, nil
	case "tui":
		if !env.recording {
			return nil, errors.New("the terminal client needs a session store; drop -sessions \"\" or use -front repl")
		}
		if !env.stdinTTY || !env.stdoutTTY {
			return nil, errors.New("the terminal client needs a terminal for standard input and output; use -front repl or -p")
		}
		return &tuiFront{info: info, pause: true}, nil
	}
	return nil, fmt.Errorf("-front %q: want tui, repl or acp", name)
}

// isTerminal reports whether f is a terminal, by asking the system:
// /dev/null and other character devices are not.
func isTerminal(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

func (f *printFront) Prepare(*agent.Options) {}
func (f *replFront) Prepare(*agent.Options)  {}

// printFront runs one prompt as an autonomous controller whose rules
// ask on standard input.
type printFront struct {
	info frontInfo
	in   *bufio.Scanner
	// stderr is where the session's line goes; nil is os.Stderr.
	stderr io.Writer
}

func (f *printFront) Run(ctx context.Context, sess *agent.Session) error {
	return f.run(ctx, sess, sess)
}

func (f *printFront) run(ctx context.Context, t agentturn.Control, ctl agent.Controls) error {
	defer t.Subscribe((&render.Printer{W: os.Stdout, Think: f.info.Think}).Handle)()
	var usage runUsage
	defer t.Subscribe(usage.count)()
	defer abortOnInterrupt(t)()
	if f.info.Timeout > 0 {
		// A deadline is a cancellation: the run ends aborted, and its
		// cause, on the record and in the end line, is the timeout.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, f.info.Timeout, &timeoutError{f.info.Timeout})
		defer cancel()
	}
	info := ctl.Info()
	stderr := f.stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	if info.Recorded {
		// On standard error, so standard output stays the run's, and
		// before the run, so a run killed at its deadline still said
		// which session it is.
		fmt.Fprintf(stderr, "session: %s\n", info.ID)
	}
	if l := agentsMDNotice(info, f.info.UserConfig); l != "" {
		// On standard error too: the model was not given instructions
		// the person may count on, which a script's output would hide.
		fmt.Fprintln(stderr, render.Clean(l))
	}
	showAssembly(os.Stdout, info)
	showPending(os.Stdout, t, "resumed")
	// The questions the person typed an answer to, by ID, and the calls
	// they refused with s, which ends the run.
	// The calls refused because nobody was left to answer, whose
	// refusal is dax's rule, not the person's.
	var answered sync.Map
	// One reader scans standard input for every answer, so a wait on a
	// line ends with the run's context (-timeout) as no answer.
	lines := make(chan string)
	go func() {
		defer close(lines)
		for f.in.Scan() {
			lines <- f.in.Text()
		}
	}()
	readLine := func() (string, bool) {
		select {
		case l, ok := <-lines:
			return l, ok
		case <-ctx.Done():
			return "", false
		}
	}
	rules := agent.Rules{
		Permit: func(c *openresponses.FunctionCall, reason string) agent.PermitAnswer {
			offer := offerOf(ctx, ctl, c.CallID)
			fmt.Print(questionOffer(c, reason, offer))
			line, ok := readLine()
			if !ok {
				fmt.Println()
				// Nobody is left to answer: the refusal is dax's rule,
				// not the person's.
				return agent.PermitAnswer{By: agentpolicy.ByPolicy}
			}
			if kind, note, ok := grantLine(line, offer); ok {
				return agent.PermitAnswer{Allow: true, Note: note, Grant: kind, Rule: offer.Rule}
			}
			allow, stop, note := answerLine(line)
			return agent.PermitAnswer{Allow: allow, Note: note, Stop: stop}
		},
		Reply: func(q agentturn.Question) agenttool.Answer {
			fmt.Print(questionText(q))
			line, ok := readLine()
			if !ok {
				fmt.Println()
				// Nobody is left to answer, which is not a no: a tool's
				// own question is cancelled, and a sub-agent's call
				// stays deferred, so its task call fails input-required
				// (agentturn RFC 0001 Composition).
				return agenttool.Answer{Action: agenttool.ActionCancel, By: agenttool.ByPolicy}
			}
			answered.Store(q.ID, true)
			return replyOf(q.Elicitation, line)
		},
		Refused: func(c *openresponses.FunctionCall) { fmt.Printf("  ✗ %s denied\n", render.Clean(c.Name)) },
		// The person at this terminal, for what they typed.
		By: func(q agentturn.Question) agentturn.Trigger {
			if _, ok := answered.LoadAndDelete(q.ID); ok {
				return agent.AtTerminal
			}
			return agentturn.Trigger{}
		},
	}
	end, err := agent.Drive(ctx, t, rules, openresponses.UserText(f.info.Prompt))
	if end != nil && f.info.Trajectory != "" {
		w, ok := t.(interface {
			WriteTrajectory(context.Context, string, agent.Price) error
		})
		if !ok {
			return errors.Join(err, errors.New("-trajectory: this session cannot be exported"))
		}
		// Written under the caller's context: -timeout's deadline has
		// passed by now for a run it ended.
		if werr := w.WriteTrajectory(context.WithoutCancel(ctx), f.info.Trajectory, f.info.Price); werr != nil {
			return errors.Join(err, werr)
		}
	}
	if end != nil && f.info.Result != "" {
		if werr := writeResult(f.info.Result, sessionIDOf(t), end, usage.total()); werr != nil {
			return errors.Join(err, werr)
		}
	}
	if err != nil {
		return err
	}
	ended(os.Stdout, t, end)
	if status := exitStatus(end); status != 0 && f.info.Result == "" {
		return &runEnded{status: status, reason: end.Reason}
	}
	return nil
}

// replFront reads lines from stdin: one goroutine reads, so a line
// typed during a run can steer it, follow it up, or answer a question.
type replFront struct {
	info frontInfo
	in   *bufio.Scanner
	// out is where the REPL writes; nil is os.Stdout.
	out io.Writer
	// lines are in's, read by one reader (readLines), and read the
	// count of them.
	reading sync.Once
	lines   chan typedLine
	read    atomic.Uint64
}

// waiting is something the REPL asked the user and waits on a line for:
// a permission of a run that ended, or a question a running call asked.
type waiting struct {
	q string
	// call is set for a permission, question for a question.
	call     *openresponses.FunctionCall
	question string
	// ask is the question's elicitation, which says whether it asks
	// for text.
	ask agenttool.Elicitation
	// shown is how many lines had been read when the question was last
	// shown: only a line read after it answers it.
	shown uint64
	// offer is the grant dax offers with a permission, or with a
	// sub-agent's asked call's question (asked is that question).
	offer agent.GrantOffer
	asked agentturn.Question
}

// typedLine is a line of the REPL's input and when it was read: its
// place among the lines read so far.
type typedLine struct {
	text string
	seq  uint64
}

// result is how a run the REPL started ended.
type result struct {
	end *agentturn.RunEnd
	err error
}

func (f *replFront) Run(ctx context.Context, sess *agent.Session) error {
	for {
		// The REPL answers the questions asked while a call runs.
		answering := sess.Answering()
		err := f.run(ctx, sess, sess)
		answering()
		var to *movedTo
		if !errors.As(err, &to) {
			return err
		}
		f.info.moved(sess, to.sess)
		sess = to.sess
	}
}

// readLines is the REPL's input, each line numbered as it is read, by
// one reader for the front's whole life.
func (f *replFront) readLines() (<-chan typedLine, *atomic.Uint64) {
	f.reading.Do(func() {
		f.lines = make(chan typedLine, 256)
		go func() {
			defer close(f.lines)
			for f.in.Scan() {
				f.lines <- typedLine{text: f.in.Text(), seq: f.read.Add(1)}
			}
		}()
	})
	return f.lines, &f.read
}

// movedTo ends a front's loop over one session for another, which a
// command opened (/new, /resume).
type movedTo struct{ sess *agent.Session }

func (*movedTo) Error() string { return "moved to another session" }

// moved is a front's move from old to n: whoever closes the session at
// the end now closes n, and old is closed here.
func (i frontInfo) moved(old, n *agent.Session) {
	if i.switched != nil {
		i.switched(n)
	}
	if err := old.Close(); err != nil {
		fmt.Fprintf(agent.Warnings(), "dax: closing session %s: %v\n", old.ID(), err)
	}
}

func (f *replFront) run(ctx context.Context, t agentturn.Control, ctl agent.Controls) error {
	w := f.out
	if w == nil {
		w = os.Stdout
	}
	defer t.Subscribe((&render.Printer{W: w, Think: f.info.Think, Compact: true}).Handle)()
	defer abortOnInterrupt(t)()
	// What the person types here is sent as theirs: a prompt, a steer
	// and the run their answers resume start under the trigger of the
	// person at this terminal (agentturn.TriggerHuman), which the record
	// writes as their words and a fold keeps verbatim. A command runs
	// under ctx, and a message dax composes (a template's, a !
	// command's output) is recorded as composed whatever its trigger.
	byPerson := agentturn.ContextWithTrigger(ctx, agent.AtTerminal)
	info := ctl.Info()
	fmt.Fprintf(w, "%s · %s %s · %s\n", f.info.Name, f.info.Provider, f.info.Model, render.Clean(f.info.Dir))
	if f.info.ModelInfo != "" {
		fmt.Fprintf(w, "model: %s\n", f.info.ModelInfo)
	}
	for _, l := range elsewhereLines(f.info) {
		fmt.Fprintln(w, render.Clean(l))
	}
	if info.Recorded {
		fmt.Fprintln(w, sessionLine(info))
	}
	if l := agentsMDNotice(info, f.info.UserConfig); l != "" {
		fmt.Fprintln(w, render.Clean(l))
	}
	showAssembly(w, info)
	showPending(w, t, "resumed")
	// The questions asked and closed while a call runs, in order, off
	// the loop's goroutine.
	var qmu sync.Mutex
	var qevents []agentturn.Event
	questions := make(chan struct{}, 1)
	defer t.Subscribe(func(_ context.Context, ev agentturn.Event) error {
		switch ev.(type) {
		case *agentturn.Question, *agentturn.QuestionClosed:
			qmu.Lock()
			qevents = append(qevents, ev)
			qmu.Unlock()
			select {
			case questions <- struct{}{}:
			default:
			}
		}
		return nil
	})()
	// Each line is numbered as it is read, ahead of the loop, so a line
	// typed before a question was shown is known for one even when the
	// loop takes it after: it never answers that question.
	// The reader outlives a move to another session (/new), whose loop
	// reads on from the same lines.
	lines, read := f.readLines()
	done := make(chan result, 1)
	running := false
	var asks []waiting             // what waits on the user, in order
	var answers []agentturn.Answer // the permissions answered so far
	var steerText string           // a prompt held while held calls are answered
	var sent string                // the prompt of the run in flight
	var sentCommand string         // the command whose message the run in flight was prompted with, if any
	prompt := func() {
		if !running && len(asks) == 0 {
			fmt.Fprint(w, "\n> ")
		}
	}
	next := func() {
		if len(asks) > 0 {
			asks[0].shown = read.Load()
			fmt.Fprint(w, asks[0].q)
		}
	}
	start := func(run func() (*agentturn.RunEnd, error)) {
		running = true
		go func() {
			end, err := run()
			done <- result{end, err}
		}()
	}
	ask := func(perms []agent.Permission) {
		for _, pm := range perms {
			if p := planOf(pm.Call); p != "" {
				// The plan, whole, before its question.
				fmt.Fprintf(w, "\nplan:\n%s\n", render.Clean(p))
			}
			o := offerOf(ctx, ctl, pm.Call.CallID)
			asks = append(asks, waiting{q: questionOffer(pm.Call, pm.Reason, o), call: pm.Call, offer: o})
		}
		answers = nil
		next()
	}
	// takeQuestions puts the questions asked since it last ran on the
	// lines waited for, and drops those closed meanwhile.
	takeQuestions := func() {
		qmu.Lock()
		evs := qevents
		qevents = nil
		qmu.Unlock()
		for _, ev := range evs {
			switch e := ev.(type) {
			case *agentturn.Question:
				o := questionOfferOf(ctx, ctl, *e)
				asks = append(asks, waiting{q: questionTextOffer(*e, o), question: e.ID, ask: e.Elicitation, offer: o, asked: *e})
				if len(asks) == 1 {
					next()
				}
			case *agentturn.QuestionClosed:
				// Answered, or given up by the call that asked it (an
				// abort): it no longer waits on a line.
				if i := slices.IndexFunc(asks, func(w waiting) bool { return w.question == e.ID }); i >= 0 {
					asks = slices.Delete(asks, i, i+1)
					if i == 0 && len(asks) > 0 {
						next()
					}
				}
			}
		}
	}
	prompt()
	for {
		select {
		case <-questions:
			takeQuestions()
		case r := <-done:
			running = false
			switch {
			case errors.Is(r.err, agent.ErrHeld):
				// The session stopped with calls held for approval: they
				// are answered first, and the prompt steers that run.
				steerText = sent
				if sentCommand != "" {
					// Its message is marked with where it came from, which
					// a steer of text would lose: it is not sent.
					fmt.Fprintf(w, "[calls wait for approval; %s was not sent: run it again once they are answered]\n", render.Clean(sentCommand))
				}
				if perms := agent.HeldPermissions(t); len(perms) > 0 {
					ask(perms)
					continue
				}
				// Every held call was held only beside a question
				// already answered: the kit releases them with no
				// answers, and the prompt steers that run.
				text := steerText
				steerText = ""
				start(func() (*agentturn.RunEnd, error) {
					if text != "" {
						if err := t.Queue(byPerson, agentturn.QueueSteer, openresponses.UserText(text)); err != nil {
							return nil, err
						}
					}
					return t.Resume(byPerson)
				})
				continue
			case r.err != nil:
				if s, ok := render.Explain(r.err, true); ok {
					fmt.Fprintln(os.Stderr, "\n"+s)
				} else {
					fmt.Fprintln(os.Stderr, "\nerror:", r.err)
				}
				if h := hint(r.err); h != "" {
					fmt.Fprintln(os.Stderr, h)
				}
			case r.end.Reason == agentturn.ReasonInputRequired:
				if perms := agent.PermissionsOf(t, r.end); len(perms) > 0 {
					ask(perms)
					continue
				}
				ended(w, t, r.end)
			default:
				ended(w, t, r.end)
			}
			prompt()
		case typed, ok := <-lines:
			if !ok {
				if running {
					t.Abort()
					<-done
				}
				fmt.Fprintln(w)
				return nil
			}
			line := strings.TrimSpace(typed.text)
			// A question shown before the line was read is what the
			// line answers, not a run to steer. One the line was typed
			// ahead of, shown since or not shown yet, is not: the line
			// goes on as what it would have been, and the question
			// waits for a line typed after it.
			takeQuestions()
			fresh := len(asks) > 0 && typed.seq > asks[0].shown
			if len(asks) > 0 && !fresh {
				if line != "" && !isCommand(line) {
					text := userText(line)
					switch {
					case running:
						if err := t.Queue(byPerson, agentturn.QueueSteer, openresponses.UserText(text)); err != nil {
							fmt.Fprintln(os.Stderr, "error:", err)
						} else {
							fmt.Fprintln(w, "\n[typed before the question: queued as steering]")
						}
					default:
						// Sent with the answers, as a prompt typed while
						// held calls wait is.
						if steerText != "" {
							steerText += "\n"
						}
						steerText += text
						fmt.Fprintln(w, "\n[typed before the question: sent with your answer]")
					}
					next()
					continue
				}
				if line == "" {
					continue
				}
				// A command typed ahead runs as a command.
			}
			if fresh {
				a := asks[0]
				asks = asks[1:]
				allow, stop, note := answerLine(line)
				if kind, gnote, ok := grantLine(line, a.offer); ok && a.question != "" && kind == agent.GrantSession {
					// "Yes, always" to a sub-agent's asked call: the
					// session grant first, then the yes.
					if g, ok := ctl.(questionGranter); ok {
						if err := g.GrantQuestion(ctx, a.asked, a.offer.Rule); err != nil {
							fmt.Fprintln(os.Stderr, "error:", err)
						} else {
							fmt.Fprintf(w, "  [%s allowed for this session]\n", render.Clean(a.offer.Rule))
							line = "y " + gnote
						}
					}
				}
				if kind, gnote, ok := grantLine(line, a.offer); ok && a.call != nil {
					allow, stop, note = true, false, gnote
					if g, ok := ctl.(granter); ok {
						if err := g.Grant(ctx, a.call.CallID, kind, a.offer.Rule); err != nil {
							fmt.Fprintln(os.Stderr, "error:", err)
						} else if kind == agent.GrantSaved {
							fmt.Fprintf(w, "  [%s kept for this project]\n", render.Clean(a.offer.Rule))
						} else {
							fmt.Fprintf(w, "  [%s allowed for this session]\n", render.Clean(a.offer.Rule))
						}
					}
				}
				switch {
				case a.question != "":
					t.Reply(agentturn.ContextWithAnswerer(ctx, agent.AtTerminal), a.question, replyOf(a.ask, line))
				case allow:
					ans := agentturn.Approve(a.call.CallID).WithBy(agentpolicy.ByHuman)
					if note != "" {
						ans = ans.WithNote(note)
					}
					answers = append(answers, ans)
				default:
					fmt.Fprintf(w, "  ✗ %s denied\n", render.Clean(a.call.Name))
					answers = append(answers, agent.Refusal(a.call.CallID, note, stop))
				}
				if len(asks) > 0 {
					next()
					continue
				}
				if a.call != nil {
					give, text := answers, steerText
					answers, steerText = nil, ""
					start(func() (*agentturn.RunEnd, error) {
						if text != "" {
							if err := t.Queue(byPerson, agentturn.QueueSteer, openresponses.UserText(text)); err != nil {
								return nil, err
							}
						}
						return t.Resume(byPerson, give...)
					})
				}
				prompt()
				continue
			}
			if line == "" {
				prompt()
				continue
			}
			if isCommand(line) {
				r, err := command(ctx, t, ctl, line, running)
				if r.out != "" {
					fmt.Fprintln(w, r.out)
				}
				if err != nil {
					fmt.Fprintln(os.Stderr, "error:", err)
				}
				if items := r.send; err == nil && len(items) > 0 {
					// A prompt template's message, sent as a typed line
					// is, marked with the file it came from.
					if running {
						if err := t.Queue(byPerson, agentturn.QueueSteer, items...); err != nil {
							fmt.Fprintln(os.Stderr, "error:", err)
						} else {
							fmt.Fprintln(w, "[queued as steering]")
						}
						continue
					}
					// Not kept as the run's prompt text, which would send
					// it again unmarked: were calls held, they are asked,
					// and the REPL says the command was not sent.
					sent, sentCommand = "", strings.Fields(line)[0]
					start(func() (*agentturn.RunEnd, error) { return t.Prompt(byPerson, items...) })
					continue
				}
				if r.quit {
					if running {
						t.Abort()
						<-done
					}
					return nil
				}
				if r.open != nil {
					// A command that opens a session waits for the run
					// to end, so none is running.
					fmt.Fprintf(w, "[session %s]\n", r.open.ID())
					return &movedTo{r.open}
				}
				if !running {
					prompt()
				}
				continue
			}
			text := userText(line)
			// The line, and the MCP resources it mentions attached.
			items, err := typedItems(ctx, ctl, text)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				if !running {
					prompt()
				}
				continue
			}
			if running {
				if err := t.Queue(byPerson, agentturn.QueueSteer, items...); err != nil {
					fmt.Fprintln(os.Stderr, "error:", err)
				} else {
					fmt.Fprintln(w, "[queued as steering]")
				}
				continue
			}
			sent, sentCommand = text, ""
			if len(items) > 1 {
				// Its attachments are marked with where they came from,
				// which a steer of the text would lose: were calls held,
				// the line is not sent again.
				sent, sentCommand = "", "the line with its @ mentions"
			}
			start(func() (*agentturn.RunEnd, error) { return t.Prompt(byPerson, items...) })
		}
	}
}

// questionOffer is what the user is asked about a call the policy asked
// about, with the grant dax offers for it: a for the session, A kept
// for the project.
func questionOffer(c *openresponses.FunctionCall, reason string, o agent.GrantOffer) string {
	if reason != "" {
		reason = " (" + reason + ")"
	}
	keys := "[y/N] "
	switch {
	case o.Rule != "" && o.Saved:
		keys = "[y/a/A/N] (a: allow " + o.Rule + " for this session, A: always in this project) "
	case o.Rule != "":
		keys = "[y/a/N] (a: allow " + o.Rule + " for this session) "
	}
	return render.Clean(fmt.Sprintf("? allow %s %s%s %s", c.Name, c.Arguments, reason, keys))
}

// planOf is the plan of a call of dax-plan's exit_plan_mode, "" for any
// other call.
func planOf(c *openresponses.FunctionCall) string {
	if c == nil || c.Name != plan.Tool {
		return ""
	}
	var a struct {
		Plan string `json:"plan"`
	}
	if json.Unmarshal([]byte(c.Arguments), &a) != nil {
		return ""
	}
	return strings.TrimSpace(a.Plan)
}

// granter is what "yes, always" asks of the session's controls: the
// session's own (agent.Session.GrantOffer and Grant); dax attach offers
// none.
type granter interface {
	GrantOffer(ctx context.Context, callID string) agent.GrantOffer
	Grant(ctx context.Context, callID string, kind agent.GrantKind, rule string) error
}

// offerOf is the grant the session offers for a pending call, none when
// it grants nothing.
func offerOf(ctx context.Context, ctl any, callID string) agent.GrantOffer {
	if g, ok := ctl.(granter); ok {
		return g.GrantOffer(ctx, callID)
	}
	return agent.GrantOffer{}
}

// grantLine reads an answer that approves with a grant: a (this
// session) or A (kept for the project), in that case, when the offer
// has it, with what follows as the note.
func grantLine(line string, o agent.GrantOffer) (kind agent.GrantKind, note string, ok bool) {
	word, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
	switch {
	case word == "a" && o.Rule != "":
		return agent.GrantSession, strings.TrimSpace(rest), true
	case word == "A" && o.Rule != "" && o.Saved:
		return agent.GrantSaved, strings.TrimSpace(rest), true
	}
	return "", "", false
}

// questionGranter is "yes, always" to a sub-agent's asked call: the
// session's own (agent.Session.QuestionOffer and GrantQuestion).
type questionGranter interface {
	QuestionOffer(ctx context.Context, q agentturn.Question) agent.GrantOffer
	GrantQuestion(ctx context.Context, q agentturn.Question, rule string) error
}

// questionOfferOf is the grant the session offers with q, none when it
// grants nothing.
func questionOfferOf(ctx context.Context, ctl any, q agentturn.Question) agent.GrantOffer {
	if g, ok := ctl.(questionGranter); ok {
		return g.QuestionOffer(ctx, q)
	}
	return agent.GrantOffer{}
}

// questionText is what the user is asked of a question a running call
// asked: a sub-agent's call the policy asks about, or a tool's own.
func questionText(q agentturn.Question) string { return questionTextOffer(q, agent.GrantOffer{}) }

// questionTextOffer is questionText with the grant dax offers for a
// sub-agent's asked call: a, for the session.
func questionTextOffer(q agentturn.Question, o agent.GrantOffer) string {
	if c := q.Call; c != nil {
		msg := q.Elicitation.Message
		if d := c.Decision; d != nil && d.Subject != "" && !strings.Contains(msg, d.Subject) {
			msg += "; about: " + d.Subject
		}
		o.Saved = false
		return questionOffer(&openresponses.FunctionCall{CallID: c.CallID, Name: c.Name, Arguments: string(c.Args)}, msg, o)
	}
	if extension.IsTextQuestion(q.Elicitation) {
		return "? " + render.Clean(q.Elicitation.Message) + " [your answer, or n] "
	}
	return "? " + render.Clean(q.Elicitation.Message) + " [y/N] "
}

// answerLine reads the user's answer to a question: y or yes allows,
// anything else refuses, and what follows the first word (or the whole
// line, for a refusal that does not start with n or no) is what the user
// said with it, which the model is told.
func answerLine(line string) (allow, stop bool, note string) {
	line = strings.TrimSpace(line)
	word, rest, _ := strings.Cut(line, " ")
	word = strings.TrimRight(strings.ToLower(word), ",:;.")
	rest = strings.TrimSpace(rest)
	switch word {
	case "y", "yes":
		return true, false, rest
	case "n", "no", "":
		return false, false, rest
	case "s", "stop":
		return false, true, rest
	}
	return false, false, line
}

// replyOf is a line's answer to a question asked while a call runs.
// For dax's question for text (extension.TextQuestion), a line that is
// not an explicit no is the answer: "blue" answers "blue", and a bare y
// or yes "yes".
func replyOf(q agenttool.Elicitation, line string) agenttool.Answer {
	if extension.IsTextQuestion(q) {
		line = strings.TrimSpace(line)
		word, rest, _ := strings.Cut(line, " ")
		switch strings.TrimRight(strings.ToLower(word), ",:;.") {
		case "n", "no", "":
			return agenttool.Answer{Action: agenttool.ActionDecline, Note: strings.TrimSpace(rest)}
		case "y", "yes":
			// "y blue" answers "blue"; a bare yes, "yes".
			line = "yes"
			if r := strings.TrimSpace(rest); r != "" {
				line = r
			}
		}
		return agenttool.Answer{Action: agenttool.ActionAccept, Content: extension.TextContent(line)}
	}
	// A question is answered while its call runs: there is no run to
	// stop from here, so s declines it as n does.
	allow, _, note := answerLine(line)
	if allow {
		return agenttool.Answer{Action: agenttool.ActionAccept, Note: note}
	}
	return agenttool.Answer{Action: agenttool.ActionDecline, Note: note}
}

func openSession(ctx context.Context, opts agent.Options, resume string) (*agent.Session, error) {
	if resume != "" {
		return agent.Resume(ctx, opts, resume)
	}
	return agent.New(ctx, opts)
}

// abortOnInterrupt makes Ctrl-C abort the run in flight; a second one,
// or one while idle, exits.
// It returns what stops it, for a front that moves to another session.
func abortOnInterrupt(t agentturn.Control) (stop func()) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-sigs:
				if !t.State().Running {
					fmt.Println()
					os.Exit(130)
				}
				t.Abort()
			}
		}
	}()
	return func() {
		signal.Stop(sigs)
		close(done)
	}
}

// ended says what a run's end leaves the user to know: the calls an
// abort cut off, which the next prompt answers, and why a run stopped.
func ended(w io.Writer, t agentturn.Control, end *agentturn.RunEnd) {
	switch end.Reason {
	case agentturn.ReasonAborted:
		showPending(w, t, "aborted")
	case agentturn.ReasonInputRequired:
		for _, p := range end.Pending {
			fmt.Fprintf(w, "[input required: %s %s]\n", render.Clean(p.Call.Name), render.Clean(p.Call.Arguments))
		}
	case agentturn.ReasonStopped:
		fmt.Fprintf(w, "[stopped: %s]\n", end.Cause)
	}
	if te := (*timeoutError)(nil); end.Reason == agentturn.ReasonAborted && errors.As(end.Err, &te) {
		fmt.Fprintf(w, "[%v]\n", te)
	}
	if h := hint(end.Err); end.Err != nil && h != "" {
		fmt.Fprintf(w, "[%s]\n", h)
	}
}

// assemblyLines are the tools the kit assembled, with the source of
// each one that is not dax's own, and what the instruction layers left
// out, so the user knows what the model was not given.
func assemblyLines(info agent.Info) []string {
	var names []string
	for _, t := range info.Tools {
		mark := t.Source
		switch {
		case t.Deferred && slices.Contains(info.Loaded, t.Name):
			mark += ", loaded"
		case t.Deferred:
			mark += ", deferred"
		}
		if mark == "WithTools" {
			names = append(names, t.Name)
		} else {
			names = append(names, t.Name+" ["+mark+"]")
		}
	}
	lines := []string{"tools: " + strings.Join(names, ", ")}
	for _, o := range info.Omitted {
		if o.Part == agentsmd.PartID && o.Reason == agentsmd.Truncated.String() {
			// A cut file is in the instructions, its first part sent.
			lines = append(lines, fmt.Sprintf("omitted: %s: its first %d bytes sent, %d bytes left out", o, info.AgentsMDKept, o.Size))
			continue
		}
		lines = append(lines, fmt.Sprintf("omitted: %s", o))
	}
	return lines
}

// agentsMDNotice is the start line every front prints, -v or not, when
// the AGENTS.md chain did not give the model a file whole for its
// budget: the file it cut, where and how much went, or the first file
// it left out whole and its size; how many files after it were left
// out; the bound and what sets it, so the person learns the model was
// not given all the files. "" when the chain sent every file whole.
func agentsMDNotice(si agent.Info, userConfig string) string {
	var first *agentkit.Omission
	cut := false
	later := 0
	for i, o := range si.Omitted {
		if o.Part != agentsmd.PartID {
			continue
		}
		switch {
		case first == nil && o.Reason == agentsmd.Truncated.String():
			first, cut = &si.Omitted[i], true
		case o.Reason != agentsmd.OverBudget.String():
		case first == nil:
			first = &si.Omitted[i]
		default:
			later++
		}
	}
	if first == nil {
		return ""
	}
	var what string
	if cut {
		what = fmt.Sprintf("cut %s at %d bytes, %d bytes left out", first.What, si.AgentsMDKept, first.Size)
		switch later {
		case 0:
		case 1:
			what += ", and left out 1 file after it"
		default:
			what += fmt.Sprintf(", and left out %d files after it", later)
		}
	} else {
		what = fmt.Sprintf("left out %s (%d bytes)", first.What, first.Size)
		switch later {
		case 0:
		case 1:
			what += " and 1 file after it"
		default:
			what += fmt.Sprintf(" and %d files after it", later)
		}
	}
	if si.AgentsMDProject > 0 {
		return fmt.Sprintf("AGENTS.md: %s, past the %d bytes the project's .dax/config.json allows its files (agents_md_budget)", what, si.AgentsMDProject)
	}
	where := "your config"
	if userConfig != "" {
		where += " (" + userConfig + ")"
	}
	return fmt.Sprintf("AGENTS.md: %s, past the chain's budget of %d bytes; agents_md_budget in %s raises it", what, si.AgentsMDBudget, where)
}

// mcpLines list the session's MCP servers, one a line: its name, where
// it runs, and what it says it is for, the same for every placement.
// Each field, a description and a served workspace's root among them,
// is text from whoever configured or offered the server or served the
// session: it is cleaned, put on one line and cut short, so it cannot
// pass for another line of the listing or act on the terminal. A nil
// list is a served session whose dax lists none, older than /mcp, which
// is not said to have none.
func mcpLines(info agent.Info) []string {
	switch {
	case info.MCP == nil:
		return []string{"mcp servers: not listed; the served session's dax is older than /mcp"}
	case len(info.MCP) == 0:
		return []string{"mcp servers: none"}
	}
	lines := []string{"mcp servers:"}
	for _, m := range info.MCP {
		l := "  " + oneLine(m.Name, maxField) + ": " + placedText(m)
		if st := stateText(m); st != "" {
			l += ", " + st
		}
		if m.Secrets {
			l += ", given secrets (secret_env)"
		}
		if m.HoldsSecrets {
			l += ", holds secrets there"
		}
		if m.Added != "" {
			l += ", added as " + oneLine(m.Added, maxField)
		}
		if t := toolsText(m); t != "" {
			l += ", " + t
		}
		if d := oneLine(m.Description, maxDescription); d != "" {
			l += " — " + d
		}
		lines = append(lines, l)
	}
	return lines
}

// maxDescription is how many columns of a server's description a
// listing shows, and maxField how many of each other field.
const (
	maxDescription = 160
	maxField       = 80
)

// toolsText says, of a server whose tools are held back until the model
// loads them (agent.MCPServerInfo.Deferred), how many and how many the
// conversation has loaded: "17 tools deferred, 2 loaded" (/tools names
// them). Nothing for a server whose tools are all sent, or a dax that
// did not say.
func toolsText(m agent.MCPServerInfo) string {
	if m.Deferred == 0 {
		return ""
	}
	t := fmt.Sprintf("%d tools deferred", m.Deferred)
	if m.Deferred == 1 {
		t = "1 tool deferred"
	}
	return fmt.Sprintf("%s, %d loaded", t, m.Loaded)
}

// stateText says where m's session is (agent.MCPServerInfo.State), or
// nothing for a dax that did not say.
func stateText(m agent.MCPServerInfo) string {
	switch m.State {
	case "":
		return ""
	case "ended":
		return "ended; the next call to one of its tools starts it again"
	case "failed":
		return "failed to start again (" + oneLine(m.Failure, maxField) + "); the next call tries again"
	}
	return oneLine(m.State, maxField)
}

// placedText says where m runs.
func placedText(m agent.MCPServerInfo) string {
	at := oneLine(m.Where, maxField)
	switch m.Placement {
	case agent.PlacedHost:
		return "on this machine"
	case agent.PlacedWorkspace:
		return "in the workspace " + at
	case agent.PlacedURL:
		return "at " + at
	case agent.PlacedOffered:
		return "offered by the workspace " + at
	case agent.PlacedServersWorkspace:
		return "offered by the servers workspace " + at
	}
	// A placement this dax does not know, from a newer one it attached
	// to: said as it is.
	return strings.TrimSpace(oneLine(string(m.Placement), maxField) + " " + at)
}

// oneLine is s cleaned (render.Clean) with its runs of white space,
// newlines and tabs among them, made one space, and cut to limit
// columns of the terminal, a wide character two, ending "…".
func oneLine(s string, limit int) string {
	s = strings.Join(strings.Fields(render.Clean(s)), " ")
	if ansi.StringWidth(s) > limit {
		s = strings.TrimRight(ansi.Truncate(s, limit-1, ""), " ") + "…"
	}
	return s
}

func showAssembly(w io.Writer, info agent.Info) {
	for _, l := range assemblyLines(info) {
		fmt.Fprintln(w, render.Clean(l))
	}
}

// showPending tells the user which calls are unanswered and why, since
// the next prompt answers them in those terms.
func showPending(w io.Writer, t agentturn.Control, why string) {
	pending := t.State().Pending
	if len(pending) == 0 {
		return
	}
	fmt.Fprintf(w, "[%s; %d tool call(s) unanswered, answered on the next prompt]\n", why, len(pending))
	for _, p := range pending {
		fmt.Fprintf(w, "  ⏸ %s\n", render.Clean(agent.Describe(p)))
	}
}
