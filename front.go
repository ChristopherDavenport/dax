package dax

// This file is the seam where the front is chosen. A front is the human
// plane of a session: it drives the session's agent.Turn (prompt,
// steer, answer, abort, the run's events and the questions asked while
// a call runs), uses dax's own agent.Controls for its slash commands
// and start lines, and may follow the session's record. Everything
// below a front, the policy, the tools, the recording, is the agent
// package's and does not know which front holds it; a front over a
// wire to a remote session would hold the same Turn.
//
// There are three: the terminal client (tui.go, agentconsole's console,
// a view of the record, over glue that presents the Turn as
// agentconsole's client.Backend), the default when standard input and
// output are a terminal; the REPL, which has the slash commands; and
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
	"strings"
	"syscall"

	"github.com/ChristopherDavenport/agentconsole/client"
	"github.com/ChristopherDavenport/agentconsole/toolview"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
	"golang.org/x/term"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/internal/render"
)

// frontInfo is what a front shows the user about the session.
type frontInfo struct {
	// Name is the program's, which the banner and the resume command
	// name.
	Name string
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
	// questions reach the user; those come through the Backend.
	Prepare(o *agent.Options)
	// Run drives the session until the user is done: the REPL and print
	// through its Turn and Controls alone, the terminal client through
	// the glue that shows it in agentconsole.
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
	return f.run(ctx, sess.Turn(), sess)
}

func (f *printFront) run(ctx context.Context, t agent.Turn, ctl agent.Controls) error {
	defer t.Subscribe((&render.Printer{W: os.Stdout, Think: f.info.Think}).Handle)()
	abortOnInterrupt(t)
	showAssembly(ctl.Info())
	showPending(t, "resumed")
	rules := agent.Rules{
		Permit: func(c *openresponses.FunctionCall, reason string) (bool, string) {
			fmt.Print(question(c, reason))
			if !f.in.Scan() {
				fmt.Println()
				return false, ""
			}
			return yes(f.in.Text()), ""
		},
		Reply: func(q agent.Question) agent.Reply {
			fmt.Print(questionText(q))
			return agent.Reply{Accept: f.in.Scan() && yes(f.in.Text())}
		},
		Refused: func(c *openresponses.FunctionCall) { fmt.Printf("  ✗ %s denied\n", render.Clean(c.Name)) },
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
}

// result is how a run the REPL started ended.
type result struct {
	end *agentturn.RunEnd
	err error
}

func (f *replFront) Run(ctx context.Context, sess *agent.Session) error {
	return f.run(ctx, sess.Turn(), sess)
}

func (f *replFront) run(ctx context.Context, t agent.Turn, ctl agent.Controls) error {
	defer t.Subscribe((&render.Printer{W: os.Stdout, Think: f.info.Think}).Handle)()
	abortOnInterrupt(t)
	info := ctl.Info()
	fmt.Printf("%s · %s %s · %s\n", f.info.Name, f.info.Provider, f.info.Model, f.info.Dir)
	if f.info.ModelInfo != "" {
		fmt.Printf("model: %s\n", f.info.ModelInfo)
	}
	if f.info.Executor != "" {
		fmt.Printf("executor: %s\n", render.Clean(f.info.Executor))
	}
	if info.Recorded {
		fmt.Printf("session %s\n", info.ID)
	}
	showAssembly(info)
	showPending(t, "resumed")
	questions := make(chan agent.Question)
	defer t.Questions(func(q agent.Question) {
		go func() {
			select {
			case questions <- q:
			case <-q.Done:
			}
		}()
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
	prompt()
	for {
		select {
		case q := <-questions:
			asks = append(asks, waiting{q: questionText(q), question: q.ID})
			if len(asks) == 1 {
				next()
			}
		case r := <-done:
			running = false
			switch {
			case errors.Is(r.err, agent.ErrHeld):
				// The session stopped with calls held for approval: they
				// are answered first, and the prompt steers that run.
				steerText = sent
				var perms []agent.Permission
				for _, pc := range t.State().Pending {
					if pc.Reason == agentturn.PendingDeferred && !pc.Dispatched {
						perms = append(perms, agent.Permission{Call: pc.Call, Reason: "held for approval when the session stopped"})
					}
				}
				ask(perms)
				continue
			case r.err != nil:
				fmt.Fprintln(os.Stderr, "\nerror:", r.err)
				if h := hint(r.err); h != "" {
					fmt.Fprintln(os.Stderr, h)
				}
			case r.end.Reason == agentturn.ReasonInputRequired:
				if perms := t.Permissions(r.end); len(perms) > 0 {
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
			if len(asks) > 0 {
				a := asks[0]
				asks = asks[1:]
				switch {
				case a.question != "":
					t.Reply(a.question, agent.Reply{Accept: yes(line)})
				case yes(line):
					answers = append(answers, agentturn.Approve(a.call.CallID).WithBy(agentpolicy.ByHuman))
				default:
					fmt.Printf("  ✗ %s denied\n", render.Clean(a.call.Name))
					answers = append(answers, agentturn.Output(openresponses.NewFunctionCallOutput(a.call.CallID, deniedOutput)).WithBy(agentpolicy.ByHuman))
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
							if err := t.Steer(ctx, openresponses.UserText(text)); err != nil {
								return nil, err
							}
						}
						return t.Answer(ctx, give...)
					})
				}
				prompt()
				continue
			}
			if line == "" {
				prompt()
				continue
			}
			if running {
				switch {
				case strings.HasPrefix(line, "/follow "):
					if err := t.FollowUp(ctx, openresponses.UserText(strings.TrimPrefix(line, "/follow "))); err != nil {
						fmt.Fprintln(os.Stderr, "error:", err)
					} else {
						fmt.Println("[queued as follow-up]")
					}
				case line == "/abort":
					t.Abort()
				default:
					if err := t.Steer(ctx, openresponses.UserText(line)); err != nil {
						fmt.Fprintln(os.Stderr, "error:", err)
					} else {
						fmt.Println("[queued as steering]")
					}
				}
				continue
			}
			if handled, err := command(ctx, t, ctl, line); handled {
				if err != nil {
					fmt.Fprintln(os.Stderr, "error:", err)
				}
				if line == "/quit" || line == "/exit" {
					return nil
				}
				prompt()
				continue
			}
			sent = line
			start(func() (*agentturn.RunEnd, error) { return t.Prompt(ctx, openresponses.UserText(line)) })
		}
	}
}

// deniedOutput is what the model is told of a call the user refused.
const deniedOutput = "Error: the user denied this call."

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
func questionText(q agent.Question) string {
	if q.Call != nil {
		return question(q.Call, q.Text)
	}
	return "? " + render.Clean(q.Text) + " [y/N] "
}

func yes(s string) bool {
	a := strings.ToLower(strings.TrimSpace(s))
	return a == "y" || a == "yes"
}

func openSession(ctx context.Context, opts agent.Options, resume string) (*agent.Session, error) {
	if resume != "" {
		return agent.Resume(ctx, opts, resume)
	}
	return agent.New(ctx, opts)
}

// abortOnInterrupt makes Ctrl-C abort the run in flight; a second one,
// or one while idle, exits.
func abortOnInterrupt(t agent.Turn) {
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

// command handles a slash command while idle; handled is false for a
// prompt.
func command(ctx context.Context, t agent.Turn, ctl agent.Controls, line string) (handled bool, err error) {
	switch {
	case strings.HasPrefix(line, "/mcp add "):
		// /mcp add <name> <command line>
		prefix, cmd, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(line, "/mcp add ")), " ")
		if !ok {
			return true, errors.New("usage: /mcp add <name> <command line>")
		}
		label, err := ctl.AddMCP(ctx, prefix, strings.TrimSpace(cmd))
		if err == nil {
			fmt.Printf("[%s added; its tools are offered from the next prompt]\n", label)
			showAssembly(ctl.Info())
		}
		return true, err
	case strings.HasPrefix(line, "/mcp remove "):
		err := ctl.RemoveMCP(strings.TrimSpace(strings.TrimPrefix(line, "/mcp remove ")))
		if err == nil {
			showAssembly(ctl.Info())
		}
		return true, err
	case line == "/tools":
		showAssembly(ctl.Info())
		return true, nil
	case line == "/quit", line == "/exit":
		return true, nil
	case line == "/session":
		info := ctl.Info()
		fmt.Println(info.ID, info.Path)
		return true, nil
	case strings.HasPrefix(line, "/model "):
		return true, ctl.SetModel(strings.TrimSpace(strings.TrimPrefix(line, "/model ")))
	case line == "/think on":
		return true, ctl.SetThink(true)
	case line == "/think off":
		return true, ctl.SetThink(false)
	case strings.HasPrefix(line, "/follow "):
		if err := t.FollowUp(ctx, openresponses.UserText(strings.TrimPrefix(line, "/follow "))); err != nil {
			return true, err
		}
		fmt.Println("[queued as follow-up for the next run]")
		return true, nil
	}
	return false, nil
}

// ended says what a run's end leaves the user to know: the calls an
// abort cut off, which the next prompt answers, and why a run stopped.
func ended(t agent.Turn, end *agentturn.RunEnd) {
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
func showPending(t agent.Turn, why string) {
	pending := t.State().Pending
	if len(pending) == 0 {
		return
	}
	fmt.Printf("[%s; %d tool call(s) unanswered, answered on the next prompt]\n", why, len(pending))
	for _, p := range pending {
		fmt.Printf("  ⏸ %s\n", render.Clean(agent.Describe(p)))
	}
}
