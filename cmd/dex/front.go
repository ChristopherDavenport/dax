package main

// This file is the seam where the front is chosen. A front is how the
// user talks to a session: it supplies the callbacks the session asks
// questions through (Hooks), then drives the session until the user is
// done (Run). Everything below a front, the policy, the tools, the
// recording, is the agent package's and does not know which front is
// attached.
//
// There are three: the terminal client (tui.go, agentconsole over the
// kit), the default when standard input and output are a terminal; the
// REPL, which has the slash commands; and print, which is -p. A new
// front implements front and gets a case in selectFront. The renderer
// for events, internal/render, is the REPL's and print's; the terminal
// client renders the session's record itself.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dex/internal/agent"
	"github.com/ChristopherDavenport/dex/internal/render"
)

// frontInfo is what a front shows the user about the session.
type frontInfo struct {
	Provider, Model, Dir string
	Think                bool
	// Prompt is the one-shot prompt of -p; empty for an interactive
	// front.
	Prompt string
	// Policy is a line saying what policy is in force.
	Policy string
}

// front is a way to talk to a session.
type front interface {
	// Hooks returns the callbacks the session is built with: how a
	// call the policy asked about is put to the user, and how a tool's
	// question mid-call is.
	Hooks() (approve func(call *openresponses.FunctionCall, reason string) bool, elicit agenttool.Elicitor)
	// Prepare adjusts the options the session is built with: a front
	// that drives the kit through its own backend asks for no agent, and
	// one that takes the screen collects what would be printed.
	Prepare(o *agent.Options)
	// Run drives the session until the user is done.
	Run(ctx context.Context, sess *agent.Session) error
}

// selectFront picks the front: print for -p; otherwise the named one,
// and with no name the terminal client when standard input and output
// are a terminal, and the REPL when they are not.
func selectFront(name, prompt string, info frontInfo) (front, error) {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	if prompt != "" {
		return &printFront{info: info, in: in}, nil
	}
	if name == "" {
		name = "repl"
		if isTerminal(os.Stdin) && isTerminal(os.Stdout) {
			name = "tui"
		}
	}
	switch name {
	case "repl":
		return &replFront{info: info, in: in, asks: make(chan *ask)}, nil
	case "tui":
		return &tuiFront{info: info, pause: isTerminal(os.Stdin)}, nil
	}
	return nil, fmt.Errorf("-front %q: want tui or repl", name)
}

// isTerminal reports whether f is a character device, which a terminal
// is and a pipe or a file is not.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func (f *printFront) Prepare(*agent.Options) {}
func (f *replFront) Prepare(*agent.Options)  {}

// printFront runs one prompt: approvals read stdin directly.
type printFront struct {
	info frontInfo
	in   *bufio.Scanner
}

func (f *printFront) Hooks() (func(*openresponses.FunctionCall, string) bool, agenttool.Elicitor) {
	approve := func(c *openresponses.FunctionCall, reason string) bool {
		fmt.Print(question(c, reason))
		if !f.in.Scan() {
			fmt.Println()
			return false
		}
		return yes(f.in.Text())
	}
	return approve, elicitor(func(q string) bool {
		fmt.Print(q)
		return f.in.Scan() && yes(f.in.Text())
	})
}

func (f *printFront) Run(ctx context.Context, sess *agent.Session) error {
	sess.Agent.Subscribe((&render.Printer{W: os.Stdout, Think: f.info.Think}).Handle)
	abortOnInterrupt(sess)
	showAssembly(sess)
	showPending(sess, "resumed")
	return turn(ctx, sess, f.info.Prompt)
}

// replFront reads lines from stdin: one goroutine reads, so a line
// typed during a run can steer it, follow it up, or answer an
// approval.
type replFront struct {
	info frontInfo
	in   *bufio.Scanner
	asks chan *ask
}

func (f *replFront) Hooks() (func(*openresponses.FunctionCall, string) bool, agenttool.Elicitor) {
	approve := func(c *openresponses.FunctionCall, reason string) bool {
		a := &ask{q: question(c, reason), reply: make(chan bool, 1)}
		f.asks <- a
		return <-a.reply
	}
	return approve, elicitor(func(q string) bool {
		a := &ask{q: q, reply: make(chan bool, 1)}
		f.asks <- a
		return <-a.reply
	})
}

func (f *replFront) Run(ctx context.Context, sess *agent.Session) error {
	sess.Agent.Subscribe((&render.Printer{W: os.Stdout, Think: f.info.Think}).Handle)
	abortOnInterrupt(sess)
	fmt.Printf("dex · %s %s · %s\n", f.info.Provider, f.info.Model, f.info.Dir)
	if id := sess.ID(); id != "" {
		fmt.Printf("session %s\n", id)
	}
	showAssembly(sess)
	showPending(sess, "resumed")
	lines := make(chan string)
	go func() {
		defer close(lines)
		for f.in.Scan() {
			lines <- f.in.Text()
		}
	}()
	done := make(chan error, 1)
	running := false
	var cur *ask
	prompt := func() {
		if !running {
			fmt.Print("\n> ")
		}
	}
	prompt()
	for {
		select {
		case a := <-f.asks:
			cur = a
			fmt.Print(a.q)
		case err := <-done:
			running = false
			if err != nil {
				fmt.Fprintln(os.Stderr, "\nerror:", err)
				if h := hint(err); h != "" {
					fmt.Fprintln(os.Stderr, h)
				}
			}
			prompt()
		case line, ok := <-lines:
			if !ok {
				if running {
					sess.Agent.Abort()
					<-done
				}
				fmt.Println()
				return nil
			}
			line = strings.TrimSpace(line)
			if cur != nil {
				cur.reply <- yes(line)
				cur = nil
				continue
			}
			if line == "" {
				prompt()
				continue
			}
			if running {
				switch {
				case strings.HasPrefix(line, "/follow "):
					sess.FollowUp(strings.TrimPrefix(line, "/follow "))
					fmt.Println("[queued as follow-up]")
				case line == "/abort":
					sess.Agent.Abort()
				default:
					sess.Steer(line)
					fmt.Println("[queued as steering]")
				}
				continue
			}
			if handled, err := command(ctx, sess, line); handled {
				if err != nil {
					fmt.Fprintln(os.Stderr, "error:", err)
				}
				if line == "/quit" || line == "/exit" {
					return nil
				}
				prompt()
				continue
			}
			running = true
			go func(text string) { done <- turn(ctx, sess, text) }(line)
		}
	}
}

type ask struct {
	q     string
	reply chan bool
}

// question is what the user is asked about a call the policy asked
// about.
func question(c *openresponses.FunctionCall, reason string) string {
	if reason != "" {
		reason = " (" + reason + ")"
	}
	return render.Clean(fmt.Sprintf("? allow %s %s%s [y/N] ", c.Name, c.Arguments, reason))
}

// elicitor puts a tool's mid-call question to the user as a yes or no.
// dex has no form to fill in, so a question asking for one is
// cancelled, as is one answered out of band at a URL, which is shown.
// Questions asked together are put one at a time.
func elicitor(confirm func(q string) bool) agenttool.Elicitor {
	var mu sync.Mutex
	return func(_ context.Context, q agenttool.Elicitation) (agenttool.Answer, error) {
		mu.Lock()
		defer mu.Unlock()
		if q.URL != "" {
			fmt.Printf("[a tool asks you to visit %s: %s]\n", render.Clean(q.URL), render.Clean(q.Message))
			return agenttool.Answer{Action: agenttool.ActionCancel}, nil
		}
		if hasFields(q.Schema) {
			fmt.Printf("[a tool asks for a form dex cannot show: %s]\n", render.Clean(q.Message))
			return agenttool.Answer{Action: agenttool.ActionCancel}, nil
		}
		if confirm("? " + render.Clean(q.Message) + " [y/N] ") {
			return agenttool.Answer{Action: agenttool.ActionAccept, Content: json.RawMessage(`{}`)}, nil
		}
		return agenttool.Answer{Action: agenttool.ActionDecline}, nil
	}
}

// hasFields reports whether a form's schema asks for any property.
func hasFields(schema json.RawMessage) bool {
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	return len(schema) > 0 && json.Unmarshal(schema, &s) == nil && len(s.Properties) > 0
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
func abortOnInterrupt(sess *agent.Session) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		for range sigs {
			if !sess.Agent.State().Running {
				fmt.Println()
				sess.Close()
				os.Exit(130)
			}
			sess.Agent.Abort()
		}
	}()
}

// command handles a slash command while idle; handled is false for a
// prompt.
func command(ctx context.Context, sess *agent.Session, line string) (handled bool, err error) {
	switch {
	case strings.HasPrefix(line, "/mcp add "):
		// /mcp add <name> <command line>
		prefix, cmd, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(line, "/mcp add ")), " ")
		if !ok {
			return true, errors.New("usage: /mcp add <name> <command line>")
		}
		label, err := sess.AddMCP(ctx, prefix, strings.TrimSpace(cmd))
		if err == nil {
			fmt.Printf("[%s added; its tools are offered from the next prompt]\n", label)
			showAssembly(sess)
		}
		return true, err
	case strings.HasPrefix(line, "/mcp remove "):
		err := sess.RemoveMCP(strings.TrimSpace(strings.TrimPrefix(line, "/mcp remove ")))
		if err == nil {
			showAssembly(sess)
		}
		return true, err
	case line == "/tools":
		showAssembly(sess)
		return true, nil
	case line == "/quit", line == "/exit":
		return true, nil
	case line == "/session":
		fmt.Println(sess.ID(), sess.Path())
		return true, nil
	case strings.HasPrefix(line, "/model "):
		return true, sess.SetModel(strings.TrimSpace(strings.TrimPrefix(line, "/model ")))
	case line == "/think on":
		return true, sess.SetThink(true)
	case line == "/think off":
		return true, sess.SetThink(false)
	case strings.HasPrefix(line, "/follow "):
		sess.FollowUp(strings.TrimPrefix(line, "/follow "))
		fmt.Println("[queued as follow-up for the next run]")
		return true, nil
	}
	return false, nil
}

func turn(ctx context.Context, sess *agent.Session, text string) error {
	end, err := sess.Prompt(ctx, text)
	if err != nil {
		return err
	}
	switch end.Reason {
	case agentturn.ReasonAborted:
		showPending(sess, "aborted")
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
	return nil
}

// assemblyLines are the tools the kit assembled, with the source of
// each one that is not dex's own, and what the instruction layers left
// out, so the user knows what the model was not given.
func assemblyLines(sess *agent.Session) []string {
	var names []string
	for _, t := range sess.Tools() {
		if t.Source == "WithTools" {
			names = append(names, t.Name)
		} else {
			names = append(names, t.Name+" ["+t.Source+"]")
		}
	}
	lines := []string{"tools: " + strings.Join(names, ", ")}
	for _, o := range sess.Omitted() {
		lines = append(lines, fmt.Sprintf("omitted: %s", o))
	}
	return lines
}

func showAssembly(sess *agent.Session) {
	for _, l := range assemblyLines(sess) {
		fmt.Println(render.Clean(l))
	}
}

// showPending tells the user which calls are unanswered and why, since
// the next prompt answers them in those terms.
func showPending(sess *agent.Session, why string) {
	pending := sess.Pending()
	if len(pending) == 0 {
		return
	}
	fmt.Printf("[%s; %d tool call(s) unanswered, answered on the next prompt]\n", why, len(pending))
	for _, p := range pending {
		fmt.Printf("  ⏸ %s\n", render.Clean(agent.Describe(p)))
	}
}
