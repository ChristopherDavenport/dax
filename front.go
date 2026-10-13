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
	"errors"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/ChristopherDavenport/agentconsole/client"
	"github.com/ChristopherDavenport/agentconsole/toolview"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
	"golang.org/x/term"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/extension"
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
	Think    bool
	// Prompt is the one-shot prompt of -p; empty for an interactive
	// front.
	Prompt string
	// Policy is a line saying what policy is in force.
	Policy string
	// Cost prices the terminal client's session pane and status line; nil
	// shows token usage but no cost.
	Cost client.Cost
	// Verbose (-v) has the terminal client print its start lines before
	// it takes the screen and what dax noted after it exits; without it,
	// the client leaves only the command that resumes the session.
	Verbose bool
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
	case "tui":
		if !env.recording {
			return nil, errors.New("the terminal client needs a session store; drop -sessions \"\" or use -front repl")
		}
		if !env.stdinTTY || !env.stdoutTTY {
			return nil, errors.New("the terminal client needs a terminal for standard input and output; use -front repl or -p")
		}
		return &tuiFront{info: info, pause: true}, nil
	}
	return nil, fmt.Errorf("-front %q: want tui or repl", name)
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
}

func (f *printFront) Run(ctx context.Context, sess *agent.Session) error {
	return f.run(ctx, sess, sess)
}

func (f *printFront) run(ctx context.Context, t agentturn.Control, ctl agent.Controls) error {
	defer t.Subscribe((&render.Printer{W: os.Stdout, Think: f.info.Think}).Handle)()
	abortOnInterrupt(t)
	showAssembly(ctl.Info())
	showPending(t, "resumed")
	// The questions the person typed an answer to, by ID.
	var answered sync.Map
	rules := agent.Rules{
		Permit: func(c *openresponses.FunctionCall, reason string) (bool, string) {
			fmt.Print(question(c, reason))
			if !f.in.Scan() {
				fmt.Println()
				return false, ""
			}
			return answerLine(f.in.Text())
		},
		Reply: func(q agentturn.Question) agenttool.Answer {
			fmt.Print(questionText(q))
			if !f.in.Scan() {
				fmt.Println()
				if q.Call == nil {
					// A tool's own question: nobody is left to answer
					// it, which is not a no.
					return agenttool.Answer{Action: agenttool.ActionCancel}
				}
				// Nobody is left to answer: the decline is dax's.
				return agenttool.Answer{Action: agenttool.ActionDecline}
			}
			answered.Store(q.ID, true)
			return replyOf(q.Elicitation, f.in.Text())
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
	if err != nil {
		return err
	}
	ended(t, end)
	return nil
}

// replFront reads lines from stdin: one goroutine reads, so a line
// typed during a run can steer it, follow it up, or answer a question.
type replFront struct {
	info frontInfo
	in   *bufio.Scanner
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
}

// result is how a run the REPL started ended.
type result struct {
	end *agentturn.RunEnd
	err error
}

func (f *replFront) Run(ctx context.Context, sess *agent.Session) error {
	// The REPL answers the questions asked while a call runs.
	defer sess.Answering()()
	return f.run(ctx, sess, sess)
}

func (f *replFront) run(ctx context.Context, t agentturn.Control, ctl agent.Controls) error {
	defer t.Subscribe((&render.Printer{W: os.Stdout, Think: f.info.Think}).Handle)()
	abortOnInterrupt(t)
	info := ctl.Info()
	fmt.Printf("%s · %s %s · %s\n", f.info.Name, f.info.Provider, f.info.Model, f.info.Dir)
	if f.info.ModelInfo != "" {
		fmt.Printf("model: %s\n", f.info.ModelInfo)
	}
	if f.info.Executor != "" {
		fmt.Println(render.Clean(executorBanner(f.info.Executor)))
	}
	if info.Recorded {
		fmt.Printf("session %s\n", info.ID)
	}
	showAssembly(info)
	showPending(t, "resumed")
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
	lines := make(chan string)
	go func() {
		defer close(lines)
		for f.in.Scan() {
			lines <- f.in.Text()
		}
	}()
	done := make(chan result, 1)
	running := false
	var asks []waiting             // what waits on the user, in order
	var answers []agentturn.Answer // the permissions answered so far
	var steerText string           // a prompt held while held calls are answered
	var sent string                // the prompt of the run in flight
	prompt := func() {
		if !running && len(asks) == 0 {
			fmt.Print("\n> ")
		}
	}
	next := func() {
		if len(asks) > 0 {
			fmt.Print(asks[0].q)
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
			asks = append(asks, waiting{q: question(pm.Call, pm.Reason), call: pm.Call})
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
				asks = append(asks, waiting{q: questionText(*e), question: e.ID, ask: e.Elicitation})
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
					if err := t.Queue(ctx, agentturn.QueueSteer, openresponses.UserText(text)); err != nil {
						return nil, err
					}
					return t.Resume(ctx)
				})
				continue
			case r.err != nil:
				fmt.Fprintln(os.Stderr, "\nerror:", r.err)
				if h := hint(r.err); h != "" {
					fmt.Fprintln(os.Stderr, h)
				}
			case r.end.Reason == agentturn.ReasonInputRequired:
				if perms := agent.PermissionsOf(t, r.end); len(perms) > 0 {
					ask(perms)
					continue
				}
				ended(t, r.end)
			default:
				ended(t, r.end)
			}
			prompt()
		case line, ok := <-lines:
			if !ok {
				if running {
					t.Abort()
					<-done
				}
				fmt.Println()
				return nil
			}
			line = strings.TrimSpace(line)
			// A question asked before the line was read is what the
			// line answers, not a run to steer.
			takeQuestions()
			if len(asks) > 0 {
				a := asks[0]
				asks = asks[1:]
				allow, note := answerLine(line)
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
					fmt.Printf("  ✗ %s denied\n", render.Clean(a.call.Name))
					answers = append(answers, agent.Refusal(a.call.CallID, note))
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
							if err := t.Queue(ctx, agentturn.QueueSteer, openresponses.UserText(text)); err != nil {
								return nil, err
							}
						}
						return t.Resume(ctx, give...)
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
					fmt.Println(r.out)
				}
				if err != nil {
					fmt.Fprintln(os.Stderr, "error:", err)
				}
				if r.quit {
					if running {
						t.Abort()
						<-done
					}
					return nil
				}
				if !running {
					prompt()
				}
				continue
			}
			text := userText(line)
			if running {
				if err := t.Queue(ctx, agentturn.QueueSteer, openresponses.UserText(text)); err != nil {
					fmt.Fprintln(os.Stderr, "error:", err)
				} else {
					fmt.Println("[queued as steering]")
				}
				continue
			}
			sent = text
			start(func() (*agentturn.RunEnd, error) { return t.Prompt(ctx, openresponses.UserText(text)) })
		}
	}
}

// question is what the user is asked about a call the policy asked
// about.
func question(c *openresponses.FunctionCall, reason string) string {
	if reason != "" {
		reason = " (" + reason + ")"
	}
	return render.Clean(fmt.Sprintf("? allow %s %s%s [y/N] ", c.Name, c.Arguments, reason))
}

// questionText is what the user is asked of a question a running call
// asked: a sub-agent's call the policy asks about, or a tool's own.
func questionText(q agentturn.Question) string {
	if c := q.Call; c != nil {
		msg := q.Elicitation.Message
		if d := c.Decision; d != nil && d.Subject != "" && !strings.Contains(msg, d.Subject) {
			msg += "; about: " + d.Subject
		}
		return question(&openresponses.FunctionCall{CallID: c.CallID, Name: c.Name, Arguments: string(c.Args)}, msg)
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
func answerLine(line string) (allow bool, note string) {
	line = strings.TrimSpace(line)
	word, rest, _ := strings.Cut(line, " ")
	word = strings.TrimRight(strings.ToLower(word), ",:;.")
	rest = strings.TrimSpace(rest)
	switch word {
	case "y", "yes":
		return true, rest
	case "n", "no", "":
		return false, rest
	}
	return false, line
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
	allow, note := answerLine(line)
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
func abortOnInterrupt(t agentturn.Control) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		for range sigs {
			if !t.State().Running {
				fmt.Println()
				os.Exit(130)
			}
			t.Abort()
		}
	}()
}

// ended says what a run's end leaves the user to know: the calls an
// abort cut off, which the next prompt answers, and why a run stopped.
func ended(t agentturn.Control, end *agentturn.RunEnd) {
	switch end.Reason {
	case agentturn.ReasonAborted:
		showPending(t, "aborted")
	case agentturn.ReasonInputRequired:
		for _, p := range end.Pending {
			fmt.Printf("[input required: %s %s]\n", render.Clean(p.Call.Name), render.Clean(p.Call.Arguments))
		}
	case agentturn.ReasonStopped:
		fmt.Printf("[stopped: %s]\n", end.Cause)
	}
	if h := hint(end.Err); end.Err != nil && h != "" {
		fmt.Printf("[%s]\n", h)
	}
}

// assemblyLines are the tools the kit assembled, with the source of
// each one that is not dax's own, and what the instruction layers left
// out, so the user knows what the model was not given.
func assemblyLines(info agent.Info) []string {
	var names []string
	for _, t := range info.Tools {
		if t.Source == "WithTools" {
			names = append(names, t.Name)
		} else {
			names = append(names, t.Name+" ["+t.Source+"]")
		}
	}
	lines := []string{"tools: " + strings.Join(names, ", ")}
	for _, o := range info.Omitted {
		lines = append(lines, fmt.Sprintf("omitted: %s", o))
	}
	return lines
}

func showAssembly(info agent.Info) {
	for _, l := range assemblyLines(info) {
		fmt.Println(render.Clean(l))
	}
}

// showPending tells the user which calls are unanswered and why, since
// the next prompt answers them in those terms.
func showPending(t agentturn.Control, why string) {
	pending := t.State().Pending
	if len(pending) == 0 {
		return
	}
	fmt.Printf("[%s; %d tool call(s) unanswered, answered on the next prompt]\n", why, len(pending))
	for _, p := range pending {
		fmt.Printf("  ⏸ %s\n", render.Clean(agent.Describe(p)))
	}
}
