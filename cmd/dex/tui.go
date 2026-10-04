package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/ChristopherDavenport/agentconsole/client"
	"github.com/ChristopherDavenport/agentconsole/client/kitbackend"
	"github.com/ChristopherDavenport/agentconsole/console"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dex/internal/agent"
	"github.com/ChristopherDavenport/dex/internal/render"
)

// tuiFront is the terminal client, agentconsole, over the kit dex
// assembled. The client builds its own agent over the kit
// (kitbackend), so the session is opened without one.
//
// Everything that asks the user is answered on its screen, and nothing
// reads standard input once it has the terminal:
//
//   - A call the policy defers is a permission the client shows, y or n,
//     answered through Control.Answer, which puts the answers through
//     the kit's engine. The reason shown is the policy's, with what it
//     was asking about added (Session.TUIConfig): the rule, the secret
//     path, the git config key, the part of a command line.
//   - A call the explore sub-agent makes that the policy asks about
//     cannot be a permission: the client answers the calls its own
//     agent's run left pending, and the sub-agent's run is not that
//     run. It is refused, with a reason that tells the model to make
//     the call itself, where it is asked; the alternative, a prompt on
//     standard input, would hang the screen.
//   - A question a tool asks mid-call (MCP elicitation) has no screen in
//     the client yet, and with no elicitor the tool takes it as a
//     cancel.
//   - A stamped call that changed since it was allowed returns an error
//     to the model, which shows in the transcript and which the model
//     answers by making the call again.
type tuiFront struct {
	info frontInfo
	// console are the options console.Run is given; tests add the pipe.
	console []console.Option
	// out is where the start lines and the notes after the run go.
	out io.Writer
	// pause waits for Enter after start lines that carry warnings or
	// omissions, which the alternate screen would otherwise hide. It is
	// set only when both standard input and output are terminals, since
	// the prompt goes to the output and the answer comes from the input.
	pause bool
	in    io.Reader
	// errOut is where held-back warnings go if the session cannot be
	// opened; nil is standard error.
	errOut io.Writer
	// run is console.Run; tests replace it.
	run func(context.Context, client.Backend, ...console.Option) error

	mu       sync.Mutex
	warnings bytes.Buffer
	log      []string
	restore  func()
}

func (f *tuiFront) Hooks() (func(*openresponses.FunctionCall, string) bool, agenttool.Elicitor) {
	return nil, nil
}

// Prepare builds the session without an agent, collects what it would
// have printed, and holds back its warnings.
func (f *tuiFront) Prepare(o *agent.Options) {
	o.NoAgent = true
	o.Log = func(format string, args ...any) {
		f.mu.Lock()
		f.log = append(f.log, fmt.Sprintf(format, args...))
		f.mu.Unlock()
	}
	f.restore = agent.CaptureWarnings(&f.warnings)
}

// banner is the lines dex shows at start in every front.
func banner(info frontInfo, sess *agent.Session) []string {
	lines := []string{fmt.Sprintf("dex · %s %s · %s", info.Provider, info.Model, info.Dir)}
	if info.ModelInfo != "" {
		lines = append(lines, "model: "+info.ModelInfo)
	}
	if id := sess.ID(); id != "" {
		lines = append(lines, "session "+id)
	}
	if info.Policy != "" {
		lines = append(lines, "policy: "+info.Policy)
	}
	return append(lines, assemblyLines(sess)...)
}

func (f *tuiFront) Run(ctx context.Context, sess *agent.Session) error {
	if f.restore != nil {
		f.restore()
	}
	out := f.out
	if out == nil {
		out = os.Stdout
	}
	lines := banner(f.info, sess)
	for _, l := range lines {
		fmt.Fprintln(out, render.Clean(l))
	}
	f.mu.Lock()
	warn := f.warnings.String()
	f.mu.Unlock()
	if warn != "" {
		fmt.Fprint(out, render.Clean(warn))
	}
	omitted := len(sess.Omitted()) > 0
	if f.pause && (warn != "" || omitted) {
		// The client takes the whole screen, and what is above it goes
		// out of sight: wait until it has been read.
		fmt.Fprint(out, "press Enter to start the terminal client ")
		in := f.in
		if in == nil {
			in = os.Stdin
		}
		bufio.NewReader(in).ReadString('\n')
	}
	// What dex noted while the client had the screen, and the session ID,
	// are printed however the client ends, a panic in it included.
	defer func() {
		r := recover()
		f.flush(out, sess)
		if r != nil {
			panic(r)
		}
	}()
	be, err := kitbackend.New(sess.Kit, kitbackend.WithConfig(sess.TUIConfig))
	if err != nil {
		return err
	}
	defer be.Close()
	run := f.run
	if run == nil {
		run = console.Run
	}
	return run(ctx, be, f.console...)
}

// flush prints the notes dex buffered and the session's ID.
func (f *tuiFront) flush(out io.Writer, sess *agent.Session) {
	f.mu.Lock()
	notes := f.log
	f.log = nil
	f.mu.Unlock()
	for _, l := range notes {
		fmt.Fprintln(out, render.Clean(l))
	}
	if id := sess.ID(); id != "" {
		fmt.Fprintln(out, "session", id)
	}
}

// Abandon is for a session that could not be opened after Prepare: the
// warnings held back for the screen are printed, since the screen is
// not coming.
func (f *tuiFront) Abandon() {
	if f.restore != nil {
		f.restore()
	}
	w := f.errOut
	if w == nil {
		w = os.Stderr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fmt.Fprint(w, render.Clean(f.warnings.String()))
	for _, l := range f.log {
		fmt.Fprintln(w, render.Clean(l))
	}
}
