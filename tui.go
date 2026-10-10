package dax

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/ChristopherDavenport/agentconsole/client"
	"github.com/ChristopherDavenport/agentconsole/console"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/internal/render"
)

// tuiFront is the terminal client, agentconsole, a view of the
// session's record that drives its Turn through the glue in
// tuiadapter.go. Everything that asks the user is answered on its screen, and
// nothing reads standard input once it has the terminal:
//
//   - A call the policy defers is a permission the client shows, y or n,
//     answered through Control.Answer, which puts the answers through
//     the kit's engine. The reason shown is the policy's, with what it
//     was asking about added by the session: the rule, the secret path,
//     the git config key, the part of a command line.
//   - A call a sub-agent (explore, task) makes that the policy asks
//     about is a question the client asks while the run goes, y or n
//     with an optional reason, answered through Control.Reply: the
//     sub-agent's run waits inside the main agent's call, so it cannot
//     be a permission, which a run that ended answers.
//   - A question a tool asks mid-call (MCP elicitation) that is yes or
//     no is asked the same way. One that is a form or a page to visit
//     has no place in a question, and the session cancels it.
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

	// warnings are what dax warns about (agent.CaptureWarnings), the
	// executor's launcher's standard error among them, held back from
	// Prepare until the client has given the screen up: those from
	// before it takes the screen are shown before, the rest after.
	warnings heldOutput

	mu      sync.Mutex
	log     []string
	restore func()
}

// heldOutput is output held back while the client has the screen,
// written to from any goroutine: the executor's launcher writes from
// its own. It keeps the last maxHeld bytes.
type heldOutput struct {
	mu      sync.Mutex
	b       []byte
	dropped bool
}

// maxHeld bounds what heldOutput keeps of a noisy launcher.
const maxHeld = 64 << 10

func (h *heldOutput) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.b = append(h.b, p...)
	if over := len(h.b) - maxHeld; over > 0 {
		h.b = h.b[over:]
		// Start at a line, not in the middle of one.
		if i := bytes.IndexByte(h.b, '\n'); i >= 0 {
			h.b = h.b[i+1:]
		}
		h.dropped = true
	}
	return len(p), nil
}

// take returns what is held, cleaned, and empties it.
func (h *heldOutput) take() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := render.Clean(string(h.b))
	if h.dropped {
		s = "[earlier output dropped]\n" + s
	}
	h.b, h.dropped = nil, false
	return s
}

// Prepare collects what the session would have printed, and holds back
// its warnings, while the client has the screen.
func (f *tuiFront) Prepare(o *agent.Options) {
	o.Log = func(format string, args ...any) {
		f.mu.Lock()
		f.log = append(f.log, fmt.Sprintf(format, args...))
		f.mu.Unlock()
	}
	f.restore = agent.CaptureWarnings(&f.warnings)
}

// executorBanner is the start line that says where the tools act when
// it is not here, in the terminal client and the REPL.
func executorBanner(executor string) string { return "executor: " + executor }

// banner is the lines dax shows at start in every front.
func banner(info frontInfo, si agent.Info) []string {
	lines := []string{fmt.Sprintf("%s · %s %s · %s", info.Name, info.Provider, info.Model, info.Dir)}
	if info.ModelInfo != "" {
		lines = append(lines, "model: "+info.ModelInfo)
	}
	if info.Executor != "" {
		lines = append(lines, executorBanner(info.Executor))
	}
	if si.Recorded {
		lines = append(lines, "session "+si.ID)
	}
	if info.Policy != "" {
		lines = append(lines, "policy: "+info.Policy)
	}
	return append(lines, assemblyLines(si)...)
}

func (f *tuiFront) Run(ctx context.Context, sess *agent.Session) error {
	si := sess.Info()
	out := f.out
	if out == nil {
		out = os.Stdout
	}
	// The start lines stay on the terminal under the client's alternate
	// screen, so they are printed only with -v, but for where the tools
	// act when that is not here: the executor's line is printed either
	// way. A warning is printed either way, and waited on, since it may
	// say the store was exposed.
	if f.info.Verbose {
		for _, l := range banner(f.info, si) {
			fmt.Fprintln(out, render.Clean(l))
		}
	} else if f.info.Executor != "" {
		fmt.Fprintln(out, render.Clean(executorBanner(f.info.Executor)))
	}
	// The warnings stay captured while the client has the screen, so
	// nothing draws over it: what comes meanwhile (the executor's
	// launcher's complaints) is printed when it exits.
	warn := f.warnings.take()
	if warn != "" {
		fmt.Fprint(out, warn)
	}
	omitted := f.info.Verbose && len(si.Omitted) > 0
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
	// What dax noted while the client had the screen (with -v), and how
	// to resume the session, are printed however the client ends, a panic
	// in it included.
	defer func() {
		r := recover()
		if f.restore != nil {
			f.restore()
			f.restore = nil
		}
		f.flush(out, si)
		if r != nil {
			panic(r)
		}
	}()
	be, err := newConsoleBackend(sess)
	if err != nil {
		return err
	}
	defer be.Close()
	run := f.run
	if run == nil {
		run = console.Run
	}
	opts := append([]console.Option{console.WithToolRenderers(f.info.Renderers)}, f.console...)
	if f.info.Cost != nil {
		opts = append(opts, console.WithCost(f.info.Cost))
	}
	return run(ctx, be, opts...)
}

// flush prints the warnings held while the client had the screen,
// with -v the notes dax buffered, and then the command that resumes
// the session.
func (f *tuiFront) flush(out io.Writer, si agent.Info) {
	fmt.Fprint(out, f.warnings.take())
	f.mu.Lock()
	notes := f.log
	f.log = nil
	f.mu.Unlock()
	if f.info.Verbose {
		for _, l := range notes {
			fmt.Fprintln(out, render.Clean(l))
		}
	}
	if si.Recorded {
		fmt.Fprintln(out, resumeLine(f.info.Name, si.ID))
	}
}

// resumeLine is what the terminal client leaves on the terminal when it
// exits, for the program called name.
func resumeLine(name, id string) string {
	return "To resume this session: " + render.Clean(name) + " -resume " + render.Clean(id)
}

// Abandon is for a session that could not be opened after Prepare: the
// warnings held back for the screen are printed, since the screen is
// not coming.
func (f *tuiFront) Abandon() {
	if f.restore != nil {
		f.restore()
		f.restore = nil
	}
	w := f.errOut
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprint(w, f.warnings.take())
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range f.log {
		fmt.Fprintln(w, render.Clean(l))
	}
}
