package tool

// The background tool: a server or a watcher the model starts in the
// workspace and comes back to, which bash, whose call ends with its
// command, cannot hold. Its processes are the session's tools': they
// end when the tools are closed, with the session (agentworkspace RFC
// 0001 §8 then ends what they started).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	workspace "github.com/ChristopherDavenport/agentworkspace"
)

const (
	// maxProcesses is the most processes one session's tool holds
	// running at once.
	maxProcesses = 8
	// processKeep is the most of a process's output kept, its last
	// bytes; processShow the most one output call returns.
	processKeep = 256 << 10
	processShow = 16 << 10
	// startSettle is how long start waits for a process's first output,
	// or its end, before it answers.
	startSettle = time.Second
)

// stopWait is how long stop, and the tool's close, wait for a process
// they ended to be reported ended.
var stopWait = 5 * time.Second

// ProcessArgs are the background tool's arguments.
type ProcessArgs struct {
	Action  string `json:"action" enum:"start,list,output,stop" desc:"start a command in the background, list the processes, read a process's new output, or stop one"`
	Command string `json:"command,omitempty" desc:"For start: the bash command line to run, decided as bash's would be"`
	ID      int    `json:"id,omitempty" desc:"For output and stop: the process's id, as start and list give it"`
	// Stamp is set by dax when the policy allowed the command without
	// asking; a value that dax did not set makes the call fail.
	Stamp string `json:"dax_stamp,omitempty" desc:"Set by dax; leave it out"`
}

// Process returns the background tool over f's workspace, which must be a
// workspace.Starter for start. A start is decided as a bash call of its
// command (the same analysis, the same rules and the same stamp), and
// list, output and stop touch only the processes this tool started;
// its facts claim says so. Closing the tool stops every process it
// holds.
func Process(f *Files, maxFile int64) agenttool.Tool {
	p := &processes{f: f, an: &Analyzer{Files: f, MaxFile: maxFile}, byID: map[int]*proc{}}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	t := agenttool.New("background", "Run a long-lived command, a dev server or a watcher, in the background in the working directory: "+
		"start it, list the processes, read a process's new output, and stop it when done. Use it instead of `cmd &` in bash.",
		p.run, agenttool.WithFacts(p.facts), agenttool.WithBarrier(), agenttool.WithCloser(p.close))
	return exactArgs[ProcessArgs](t)
}

type processes struct {
	f      *Files
	an     *Analyzer
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	next   int
	byID   map[int]*proc
	closed bool
}

// proc is one started process and what it printed.
type proc struct {
	id      int
	command string
	started time.Time
	p       workspace.Process

	// redact says it runs an auto-allowed plan, whose output loses a
	// URL's credentials, as bash's does.
	redact bool

	mu      sync.Mutex
	out     []byte // the last processKeep bytes of its output
	total   int64  // every byte it printed
	read    int64  // what output calls have returned, by total
	done    chan struct{}
	status  int
	waitErr error
}

func (r *proc) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.out = append(r.out, b...)
	if len(r.out) > processKeep {
		r.out = slices.Clone(r.out[len(r.out)-processKeep:])
	}
	r.total += int64(len(b))
	return len(b), nil
}

// facts claims a start as bash's calls for its command, and its stamp
// for the arguments; list, output and stop as the call itself, which
// the process rules match by action.
func (p *processes) facts(ctx context.Context, args json.RawMessage) (agenttool.Facts, error) {
	if err := exactKeys(args, "action", "command", "id", "dax_stamp"); err != nil {
		return agenttool.Facts{}, err
	}
	var in ProcessArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return agenttool.Facts{}, err
	}
	if in.Action != "start" {
		return agenttool.Facts{}, nil
	}
	bargs, err := json.Marshal(map[string]string{"command": in.Command})
	if err != nil {
		return agenttool.Facts{}, err
	}
	calls, rewrite, err := bashFacts(ctx, p.an, bargs, true)
	if err != nil {
		return agenttool.Facts{}, err
	}
	// Stages bash's claim gives as the claiming tool's are bash's here,
	// so bash's rules decide the line.
	for i := range calls {
		if calls[i].Tool == "" {
			calls[i].Tool = "bash"
		}
	}
	var stamped struct {
		Stamp string `json:"dax_stamp"`
	}
	if rewrite == nil || json.Unmarshal(rewrite, &stamped) != nil || stamped.Stamp == "" {
		return agenttool.Facts{Calls: calls}, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return agenttool.Facts{}, err
	}
	s, _ := json.Marshal(stamped.Stamp)
	m["dax_stamp"] = s
	out, err := json.Marshal(m)
	return agenttool.Facts{Calls: calls, Rewrite: out}, err
}

func (p *processes) run(ctx context.Context, in ProcessArgs) (string, error) {
	switch in.Action {
	case "start":
		return p.start(ctx, in)
	case "list":
		return p.list(), nil
	case "output":
		r, err := p.get(in.ID)
		if err != nil {
			return "", err
		}
		return r.report(true), nil
	case "stop":
		r, err := p.get(in.ID)
		if err != nil {
			return "", err
		}
		r.p.Close()
		select {
		case <-r.done:
		case <-time.After(stopWait):
			return r.report(true) + "\n[stopped, but the workspace did not say it ended]", nil
		}
		return r.report(true), nil
	}
	return "", fmt.Errorf("action %q: want start, list, output or stop", in.Action)
}

func (p *processes) start(ctx context.Context, in ProcessArgs) (string, error) {
	if strings.TrimSpace(in.Command) == "" {
		return "", errors.New("command is required to start a process")
	}
	st, ok := p.f.Workspace().(workspace.Starter)
	if !ok {
		return "", errors.New("this workspace cannot run a process in the background; run the command with bash")
	}
	cmd, plan, err := command(ctx, p.an, BashArgs{Command: in.Command, Stamp: in.Stamp})
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	running := 0
	for _, r := range p.byID {
		if !r.exited() {
			running++
		}
	}
	switch {
	case p.closed:
		p.mu.Unlock()
		return "", errors.New("the session's tools are closed")
	case running >= maxProcesses:
		p.mu.Unlock()
		return "", fmt.Errorf("%d processes are running; stop one first", running)
	}
	p.next++
	r := &proc{id: p.next, command: in.Command, started: time.Now(), done: make(chan struct{}), redact: plan}
	p.byID[r.id] = r
	p.mu.Unlock()

	cmd.Stream = r
	// It outlives the call that started it: it ends with stop, or the
	// tool's close.
	proc, err := st.Start(p.ctx, cmd)
	if err != nil {
		p.mu.Lock()
		delete(p.byID, r.id)
		p.mu.Unlock()
		return "", err
	}
	r.p = proc
	proc.Stdin().Close()
	go func() {
		io.Copy(r, proc.Stdout())
		status, err := proc.Wait()
		r.mu.Lock()
		r.status, r.waitErr = status, err
		r.mu.Unlock()
		close(r.done)
	}()
	select {
	case <-r.done:
	case <-time.After(startSettle):
	case <-ctx.Done():
	}
	return r.report(true), nil
}

func (p *processes) get(id int) (*proc, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.byID[id]
	if !ok {
		return nil, fmt.Errorf("no process %d; list them with action list", id)
	}
	return r, nil
}

func (p *processes) list() string {
	p.mu.Lock()
	ids := make([]int, 0, len(p.byID))
	for id := range p.byID {
		ids = append(ids, id)
	}
	p.mu.Unlock()
	if len(ids) == 0 {
		return "no processes"
	}
	slices.Sort(ids)
	var b strings.Builder
	for _, id := range ids {
		r, _ := p.get(id)
		fmt.Fprintf(&b, "%s\n", r.line())
	}
	return strings.TrimRight(b.String(), "\n")
}

func (r *proc) exited() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// line is the process's one-line state.
func (r *proc) line() string {
	state := "running for " + time.Since(r.started).Round(time.Second).String()
	if r.exited() {
		r.mu.Lock()
		state = "exited " + strconv.Itoa(r.status)
		r.mu.Unlock()
	}
	return fmt.Sprintf("[%d] %s: %s", r.id, state, oneLineCommand(r.command))
}

// report is the process's state and the output printed since the last
// report, its last processShow bytes when there is more.
func (r *proc) report(advance bool) string {
	head := r.line()
	r.mu.Lock()
	defer r.mu.Unlock()
	fresh := r.total - r.read
	if fresh > int64(len(r.out)) {
		fresh = int64(len(r.out))
	}
	text := r.out[int64(len(r.out))-fresh:]
	cut := 0
	if len(text) > processShow {
		cut = len(text) - processShow
		text = text[cut:]
	}
	if advance {
		r.read = r.total
	}
	if r.redact {
		text = []byte(redactUserinfo(string(text)))
	}
	var b strings.Builder
	b.WriteString(head)
	switch {
	case cut > 0:
		fmt.Fprintf(&b, "\n[%d bytes of new output left out; the last %d follow]\n", cut, len(text))
	case len(text) > 0:
		b.WriteByte('\n')
	default:
		b.WriteString("\n[no new output]")
	}
	b.Write(text)
	return strings.TrimRight(b.String(), "\n")
}

func oneLineCommand(c string) string {
	c = strings.Join(strings.Fields(c), " ")
	if len(c) > 120 {
		c = c[:120] + "…"
	}
	return c
}

// close stops every process the tool holds, and refuses later starts.
func (p *processes) close() error {
	p.mu.Lock()
	p.closed = true
	all := make([]*proc, 0, len(p.byID))
	for _, r := range p.byID {
		all = append(all, r)
	}
	p.mu.Unlock()
	for _, r := range all {
		if r.p == nil {
			continue
		}
		r.p.Close()
		// Its output drained and its status read, so a list after the
		// close says it ended; a wire that never says so is let go.
		select {
		case <-r.done:
		case <-time.After(stopWait):
		}
	}
	p.cancel()
	return nil
}
