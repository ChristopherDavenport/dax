package executor

// The executor's processes: a process that lives longer than one call,
// with pipes to it, started in the executor's workspace for the session
// at the other end of its pipe, so that an MCP stdio server the user
// configured runs where the tools act and not on the session's machine.
//
// They are custom JSON-RPC methods, not tools: a tool would be offered
// to the model of any harness connected to `dax execute`, and this is
// arbitrary execution. The session (Remote.Start) is the only caller,
// and what it starts is what the user's configuration, flags and
// /mcp add name; the executor holds no list of its own and reads no
// config.
//
//	dax/process.start      {args, dir?, env?, timeoutMs?, stderr?} -> {id}
//	dax/process.write      {id, data (base64, at most MaxWriteBytes)} -> {}
//	dax/process.read       {id, fd: 1|2, max?} -> {data?, eof?}
//	dax/process.closeStdin {id} -> {}
//	dax/process.signal     {id, signal: INT|TERM|KILL} -> {done?}
//	dax/process.wait       {id} -> {status, ended?, error?}
//	dax/process.close      {id} -> {status, ended?, error?, stderr?}
//
// A read is a long poll: it returns as soon as there is output, at most
// max bytes (at most MaxReadBytes), or end of file, and waits otherwise.
// A wait waits for the process to exit. A close ends the process and
// gives back the standard error no read took. Each session has its own table
// of processes, at most MaxProcesses open at once: an id another
// session started is refused as one that does not exist. When the
// session ends (its connection closes), every process it started ends
// with it, and so does everything when the executor's workspace closes
// (workspace.Local.Close).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	workspace "github.com/ChristopherDavenport/agentworkspace"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The process methods.
const (
	MethodStart      = "dax/process.start"
	MethodWrite      = "dax/process.write"
	MethodRead       = "dax/process.read"
	MethodCloseStdin = "dax/process.closeStdin"
	MethodSignal     = "dax/process.signal"
	MethodWait       = "dax/process.wait"
	MethodClose      = "dax/process.close"
)

// The bounds of the process methods: processes open at once in one
// session, bytes in one write and in one read's reply.
const (
	MaxProcesses  = 32
	MaxWriteBytes = 1 << 20
	MaxReadBytes  = 64 << 10
)

// outputBytes bounds what the executor holds of a process's output
// that the session has not read yet. Standard output past it waits, as
// it would on a full pipe; standard error past it is dropped, so a
// process never stops for diagnostics nobody reads.
const outputBytes = 64 << 10

// processTimeout bounds one request of the session's other than a
// write, a read or a wait, which wait on the process. A var for the
// tests.
var processTimeout = 30 * time.Second

// capabilityStart says the executor answers the process methods, with
// its bounds; nil from an executor older than them or whose workspace
// cannot start a process.
type capabilityStart struct {
	MaxProcesses  int `json:"maxProcesses"`
	MaxWriteBytes int `json:"maxWriteBytes"`
	MaxReadBytes  int `json:"maxReadBytes"`
}

// The methods' parameters and results.
type (
	startParams struct {
		sdk.ParamsBase
		Args      []string `json:"args"`
		Dir       string   `json:"dir,omitempty"`
		Env       []string `json:"env,omitempty"`
		TimeoutMs int64    `json:"timeoutMs,omitempty"`
		// Stderr asks for the process's standard error, read as fd 2;
		// without it, it is discarded.
		Stderr bool `json:"stderr,omitempty"`
	}
	startResult struct {
		sdk.ResultBase
		ID string `json:"id"`
	}
	idParams struct {
		sdk.ParamsBase
		ID string `json:"id"`
	}
	writeParams struct {
		sdk.ParamsBase
		ID   string `json:"id"`
		Data []byte `json:"data"`
	}
	readParams struct {
		sdk.ParamsBase
		ID  string `json:"id"`
		FD  int    `json:"fd"`
		Max int    `json:"max,omitempty"`
	}
	readResult struct {
		sdk.ResultBase
		Data []byte `json:"data,omitempty"`
		EOF  bool   `json:"eof,omitempty"`
	}
	signalParams struct {
		sdk.ParamsBase
		ID     string `json:"id"`
		Signal string `json:"signal"`
	}
	signalResult struct {
		sdk.ResultBase
		// Done says the process had already exited.
		Done bool `json:"done,omitempty"`
	}
	waitResult struct {
		sdk.ResultBase
		Status int `json:"status"`
		// Ended is "timeout" when the start's timeout ended it and
		// "canceled" when the session's end did.
		Ended string `json:"ended,omitempty"`
		Error string `json:"error,omitempty"`
		// Stderr is, from a close, the standard error no read took.
		Stderr []byte `json:"stderr,omitempty"`
	}
	emptyResult struct{ sdk.ResultBase }
)

const (
	endedTimeout  = "timeout"
	endedCanceled = "canceled"
)

// signals are the signals the session may send, by name.
var signals = map[string]syscall.Signal{"INT": syscall.SIGINT, "TERM": syscall.SIGTERM, "KILL": syscall.SIGKILL}

// errNoProcess is a process id this session did not start, or closed.
var errNoProcess = errors.New("no such process in this session")

// processServer answers the process methods for one executor.
type processServer struct {
	ws   workspace.Workspace // a workspace.Starter
	next atomic.Uint64

	mu      sync.Mutex
	stopped bool
	tables  map[*sdk.ServerSession]*processTable
}

// serveProcesses registers the process methods on srv, starting
// processes in ws, a workspace.Starter. A closed done ends every
// process and every request waiting on one, so that a session's close,
// which waits for the requests in flight, is not held by a read
// waiting for output.
func serveProcesses(srv *sdk.Server, ws workspace.Workspace, done <-chan struct{}) error {
	s := &processServer{ws: ws, tables: map[*sdk.ServerSession]*processTable{}}
	if err := errors.Join(
		sdk.AddReceivingCustomMethod(srv, MethodStart, s.start),
		sdk.AddReceivingCustomMethod(srv, MethodWrite, s.write),
		sdk.AddReceivingCustomMethod(srv, MethodRead, s.read),
		sdk.AddReceivingCustomMethod(srv, MethodCloseStdin, s.closeStdin),
		sdk.AddReceivingCustomMethod(srv, MethodSignal, s.signal),
		sdk.AddReceivingCustomMethod(srv, MethodWait, s.wait),
		sdk.AddReceivingCustomMethod(srv, MethodClose, s.close),
	); err != nil {
		return err
	}
	if done != nil {
		go func() {
			<-done
			s.stop()
		}()
	}
	return nil
}

// processTable is one session's processes.
type processTable struct {
	ctx    context.Context // every process's; ended with the table
	cancel context.CancelFunc

	mu     sync.Mutex
	closed bool
	procs  map[string]*started
}

// started is a process the executor started.
type started struct {
	p       workspace.Process
	writeMu sync.Mutex // one write at a time, and the close of stdin after them
	stdout  *outputBuffer
	stderr  *outputBuffer // nil when the session did not ask for it
	gone    chan struct{} // closed when the process is closed, ending the polls
	goneOne sync.Once

	exited chan struct{} // closed once status and err are set
	status int
	err    error
}

// table is ss's table, made on its first start; a session's end closes
// it.
func (s *processServer) table(ss *sdk.ServerSession) (*processTable, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil, errors.New("the executor is stopping")
	}
	if t, ok := s.tables[ss]; ok {
		return t, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	t := &processTable{ctx: ctx, cancel: cancel, procs: map[string]*started{}}
	s.tables[ss] = t
	go func() {
		ss.Wait()
		s.mu.Lock()
		delete(s.tables, ss)
		s.mu.Unlock()
		t.close()
	}()
	return t, nil
}

// stop closes every session's table and refuses another.
func (s *processServer) stop() {
	s.mu.Lock()
	s.stopped = true
	tables := make([]*processTable, 0, len(s.tables))
	for _, t := range s.tables {
		tables = append(tables, t)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, t := range tables {
		wg.Go(t.close)
	}
	wg.Wait()
}

// lookup is ss's process id.
func (s *processServer) lookup(ss *sdk.ServerSession, id string) (*started, error) {
	s.mu.Lock()
	t := s.tables[ss]
	s.mu.Unlock()
	if t != nil {
		t.mu.Lock()
		sp := t.procs[id]
		t.mu.Unlock()
		if sp != nil {
			return sp, nil
		}
	}
	return nil, fmt.Errorf("process %q: %w", id, errNoProcess)
}

// close ends every process in t and refuses another.
func (t *processTable) close() {
	t.mu.Lock()
	t.closed = true
	procs := slices.Collect(maps.Values(t.procs))
	clear(t.procs)
	t.mu.Unlock()
	var wg sync.WaitGroup
	for _, sp := range procs {
		wg.Go(sp.close)
	}
	wg.Wait()
	t.cancel()
}

// close ends the process and the polls waiting on it.
func (sp *started) close() {
	sp.goneOne.Do(func() { close(sp.gone) })
	sp.p.Close()
	<-sp.exited
}

func (s *processServer) start(_ context.Context, ss *sdk.ServerSession, p *startParams) (*startResult, error) {
	switch {
	case p == nil || len(p.Args) == 0 || p.Args[0] == "":
		return nil, errors.New("no command")
	case p.TimeoutMs < 0:
		return nil, errors.New("a negative timeout")
	}
	for _, e := range p.Env {
		if !strings.Contains(e, "=") {
			return nil, fmt.Errorf("environment entry %q is not NAME=value", e)
		}
	}
	t, err := s.table(ss)
	if err != nil {
		return nil, err
	}
	cmd := workspace.Command{Args: p.Args, Dir: p.Dir, Env: p.Env, Timeout: time.Duration(p.TimeoutMs) * time.Millisecond}
	sp := &started{stdout: newOutputBuffer(), gone: make(chan struct{}), exited: make(chan struct{})}
	if p.Stderr {
		sp.stderr = newOutputBuffer()
		cmd.Stream = sp.stderr
	}
	// The table is held across the start, which returns at once, so
	// the count and a session's end see every process.
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case t.closed:
		return nil, errors.New("the session is ending")
	case len(t.procs) >= MaxProcesses:
		return nil, fmt.Errorf("%d processes are open in this session, the most there may be; close one first", MaxProcesses)
	}
	proc, err := workspace.Start(t.ctx, s.ws, cmd)
	if err != nil {
		return nil, err
	}
	sp.p = proc
	go sp.stdout.fill(proc.Stdout(), sp.gone)
	go func() {
		status, err := proc.Wait()
		sp.status, sp.err = status, err
		if sp.stderr != nil {
			// Wait returns once standard error is copied.
			sp.stderr.end()
		}
		close(sp.exited)
	}()
	id := fmt.Sprintf("p%d", s.next.Add(1))
	t.procs[id] = sp
	return &startResult{ID: id}, nil
}

func (s *processServer) write(_ context.Context, ss *sdk.ServerSession, p *writeParams) (*emptyResult, error) {
	if p == nil {
		return nil, errors.New("no parameters")
	}
	if len(p.Data) > MaxWriteBytes {
		return nil, fmt.Errorf("a write of %d bytes; at most %d", len(p.Data), MaxWriteBytes)
	}
	sp, err := s.lookup(ss, p.ID)
	if err != nil {
		return nil, err
	}
	// A write waits while the process does not read, as on a pipe; the
	// process's close ends it.
	sp.writeMu.Lock()
	defer sp.writeMu.Unlock()
	if _, err := sp.p.Stdin().Write(p.Data); err != nil {
		return nil, err
	}
	return &emptyResult{}, nil
}

func (s *processServer) read(ctx context.Context, ss *sdk.ServerSession, p *readParams) (*readResult, error) {
	if p == nil {
		return nil, errors.New("no parameters")
	}
	sp, err := s.lookup(ss, p.ID)
	if err != nil {
		return nil, err
	}
	var b *outputBuffer
	switch p.FD {
	case 1:
		b = sp.stdout
	case 2:
		if b = sp.stderr; b == nil {
			return nil, errors.New("standard error was not asked for at the start")
		}
	default:
		return nil, fmt.Errorf("fd %d: want 1 or 2", p.FD)
	}
	n := p.Max
	if n <= 0 || n > MaxReadBytes {
		n = MaxReadBytes
	}
	data, eof, err := b.take(ctx, n, sp.gone)
	if err != nil {
		return nil, err
	}
	return &readResult{Data: data, EOF: eof}, nil
}

func (s *processServer) closeStdin(_ context.Context, ss *sdk.ServerSession, p *idParams) (*emptyResult, error) {
	if p == nil {
		return nil, errors.New("no parameters")
	}
	sp, err := s.lookup(ss, p.ID)
	if err != nil {
		return nil, err
	}
	sp.writeMu.Lock()
	defer sp.writeMu.Unlock()
	if err := sp.p.Stdin().Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		return nil, err
	}
	return &emptyResult{}, nil
}

func (s *processServer) signal(_ context.Context, ss *sdk.ServerSession, p *signalParams) (*signalResult, error) {
	if p == nil {
		return nil, errors.New("no parameters")
	}
	sig, ok := signals[p.Signal]
	if !ok {
		return nil, fmt.Errorf("signal %q: want INT, TERM or KILL", p.Signal)
	}
	sp, err := s.lookup(ss, p.ID)
	if err != nil {
		return nil, err
	}
	if err := sp.p.Signal(sig); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return &signalResult{Done: true}, nil
		}
		return nil, err
	}
	return &signalResult{}, nil
}

func (s *processServer) wait(ctx context.Context, ss *sdk.ServerSession, p *idParams) (*waitResult, error) {
	if p == nil {
		return nil, errors.New("no parameters")
	}
	sp, err := s.lookup(ss, p.ID)
	if err != nil {
		return nil, err
	}
	select {
	case <-sp.exited:
		return sp.result(), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *processServer) close(_ context.Context, ss *sdk.ServerSession, p *idParams) (*waitResult, error) {
	if p == nil {
		return nil, errors.New("no parameters")
	}
	s.mu.Lock()
	t := s.tables[ss]
	s.mu.Unlock()
	var sp *started
	if t != nil {
		t.mu.Lock()
		sp = t.procs[p.ID]
		delete(t.procs, p.ID)
		t.mu.Unlock()
	}
	if sp == nil {
		return nil, fmt.Errorf("process %q: %w", p.ID, errNoProcess)
	}
	sp.close()
	r := sp.result()
	if sp.stderr != nil {
		r.Stderr = sp.stderr.rest()
	}
	return r, nil
}

// result is how the process ended, once it has.
func (sp *started) result() *waitResult {
	r := &waitResult{Status: sp.status}
	switch {
	case errors.Is(sp.err, context.DeadlineExceeded):
		r.Ended = endedTimeout
	case errors.Is(sp.err, context.Canceled):
		r.Ended = endedCanceled
	case sp.err != nil:
		r.Error = sp.err.Error()
	}
	return r
}

// outputBuffer is what a process wrote to one of its pipes that the
// session has not read yet, at most outputBytes: standard output fills
// it and waits for room (fill), standard error writes to it and loses
// what does not fit (Write).
type outputBuffer struct {
	mu      sync.Mutex
	buf     []byte
	eof     bool
	changed chan struct{} // closed and replaced at every change
}

func newOutputBuffer() *outputBuffer {
	return &outputBuffer{changed: make(chan struct{})}
}

// notify wakes whoever waits on a change; b.mu is held.
func (b *outputBuffer) notify() {
	close(b.changed)
	b.changed = make(chan struct{})
}

// fill copies r into b until r ends or gone closes, waiting for room.
func (b *outputBuffer) fill(r io.Reader, gone <-chan struct{}) {
	defer b.end()
	tmp := make([]byte, 32<<10)
	for {
		b.mu.Lock()
		for len(b.buf) >= outputBytes {
			ch := b.changed
			b.mu.Unlock()
			select {
			case <-ch:
			case <-gone:
				return
			}
			b.mu.Lock()
		}
		room := outputBytes - len(b.buf)
		b.mu.Unlock()
		n, err := r.Read(tmp[:min(room, len(tmp))])
		if n > 0 {
			b.mu.Lock()
			b.buf = append(b.buf, tmp[:n]...)
			b.notify()
			b.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// Write is standard error's: what does not fit is dropped.
func (b *outputBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := outputBytes - len(b.buf); room > 0 {
		b.buf = append(b.buf, p[:min(room, len(p))]...)
		b.notify()
	}
	return len(p), nil
}

// end is end of file after what b holds.
func (b *outputBuffer) end() {
	b.mu.Lock()
	b.eof = true
	b.notify()
	b.mu.Unlock()
}

// rest takes what b holds.
func (b *outputBuffer) rest() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.buf
	b.buf = nil
	b.notify()
	return out
}

// take waits for output or its end, and returns at most n bytes of it,
// and whether that is all there will be.
func (b *outputBuffer) take(ctx context.Context, n int, gone <-chan struct{}) ([]byte, bool, error) {
	for {
		b.mu.Lock()
		if len(b.buf) > 0 || b.eof {
			k := min(n, len(b.buf))
			out := slices.Clone(b.buf[:k])
			b.buf = append(b.buf[:0], b.buf[k:]...)
			eof := b.eof && len(b.buf) == 0
			b.notify()
			b.mu.Unlock()
			return out, eof, nil
		}
		ch := b.changed
		b.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-gone:
			return nil, false, errors.New("the process was closed")
		}
	}
}

// registerProcessMethods registers the process methods on c, the
// client Connect creates, before it connects.
func registerProcessMethods(c *sdk.Client) error {
	return errors.Join(
		sdk.AddSendingCustomMethod[*startParams, *startResult](c, MethodStart),
		sdk.AddSendingCustomMethod[*writeParams, *emptyResult](c, MethodWrite),
		sdk.AddSendingCustomMethod[*readParams, *readResult](c, MethodRead),
		sdk.AddSendingCustomMethod[*idParams, *emptyResult](c, MethodCloseStdin),
		sdk.AddSendingCustomMethod[*signalParams, *signalResult](c, MethodSignal),
		sdk.AddSendingCustomMethod[*idParams, *waitResult](c, MethodWait),
		sdk.AddSendingCustomMethod[*idParams, *waitResult](c, MethodClose),
	)
}

// Start starts cmd in the executor's workspace and returns at once:
// workspace.Starter's Start, over the connection. The process's
// standard input and output are requests to the executor, a write at a
// time and a read at a time; its standard error, when cmd.Stream is
// set, is read in the background and written to cmd.Stream as it
// arrives. It ends with Close, the end of ctx, cmd.Timeout, the
// connection's close or the executor's.
//
// An executor that does not answer the process methods (no start in
// its capability: an older dax execute, or a workspace there that
// cannot start a process) is refused with an error that is
// errors.ErrUnsupported; nothing is started anywhere else.
func (r *Remote) Start(ctx context.Context, cmd workspace.Command) (workspace.Process, error) {
	if r.start == nil {
		name, version := r.Server()
		return nil, fmt.Errorf("executor (%s %s) cannot start a process (no start in %s); update dax execute where it runs: %w", name, version, CapabilityKey, errors.ErrUnsupported)
	}
	switch {
	case len(cmd.Args) == 0:
		return nil, errors.New("executor: no command")
	case cmd.Stdin != nil:
		return nil, errors.New("executor: Start takes no Command.Stdin; write to Process.Stdin")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sctx, cancel := context.WithTimeout(ctx, processTimeout)
	defer cancel()
	res, err := sdk.CallCustomMethod[*startParams, *startResult](sctx, r.c.Session(), MethodStart, &startParams{
		Args: cmd.Args, Dir: cmd.Dir, Env: cmd.Env, TimeoutMs: cmd.Timeout.Milliseconds(), Stderr: cmd.Stream != nil,
	})
	if err != nil {
		return nil, fmt.Errorf("executor: start %s: %w", cmd.Args[0], err)
	}
	p := &remoteProcess{
		cs: r.c.Session(), id: res.ID,
		maxWrite: min(MaxWriteBytes, r.start.MaxWriteBytes), maxRead: min(MaxReadBytes, r.start.MaxReadBytes),
		waited: make(chan struct{}), pumped: make(chan struct{}),
	}
	p.ctx, p.cancel = context.WithCancel(r.procs)
	p.in.p, p.out.p = p, p
	go p.waitFor()
	if p.stream = cmd.Stream; p.stream != nil {
		go p.pump()
	} else {
		close(p.pumped)
	}
	stop := context.AfterFunc(ctx, func() {
		p.mu.Lock()
		select {
		case <-p.waited:
		default:
			p.ending = ctx.Err()
		}
		p.mu.Unlock()
		p.Close()
	})
	p.mu.Lock()
	p.stopWatch = stop
	p.mu.Unlock()
	return p, nil
}

// remoteProcess is a process the executor started for this session.
type remoteProcess struct {
	cs                *sdk.ClientSession
	id                string
	maxWrite, maxRead int

	ctx    context.Context // the polls'; ended by Close and the Remote's
	cancel context.CancelFunc

	in  remoteStdin
	out remoteStdout

	stream io.Writer     // the start's Command.Stream
	pumped chan struct{} // closed once standard error's pump is done

	mu        sync.Mutex
	stopWatch func() bool // stops the watch on the start's context
	ending    error       // the start context's error, once it ended the process
	waited    chan struct{}
	waitOne   sync.Once
	status    int
	err       error

	closeOne sync.Once
	closeErr error
}

var _ workspace.Process = (*remoteProcess)(nil)

func (p *remoteProcess) Stdin() io.WriteCloser { return &p.in }
func (p *remoteProcess) Stdout() io.ReadCloser { return &p.out }

// finish records how the process ended, the first time.
func (p *remoteProcess) finish(r *waitResult, err error) {
	p.waitOne.Do(func() {
		switch {
		case err != nil:
			p.status, p.err = -1, err
		default:
			p.status = r.Status
			switch r.Ended {
			case endedTimeout:
				p.err = context.DeadlineExceeded
			case endedCanceled:
				p.err = context.Canceled
			case "":
				if r.Error != "" {
					p.err = errors.New(r.Error)
				}
			}
		}
		close(p.waited)
	})
}

// waitFor waits in the executor for the process to exit.
func (p *remoteProcess) waitFor() {
	r, err := sdk.CallCustomMethod[*idParams, *waitResult](p.ctx, p.cs, MethodWait, &idParams{ID: p.id})
	if err != nil && p.ctx.Err() != nil {
		// Close ended the wait; its reply says how the process ended.
		return
	}
	p.finish(r, err)
}

// pump writes standard error to the stream as it arrives.
func (p *remoteProcess) pump() {
	defer close(p.pumped)
	for {
		r, err := sdk.CallCustomMethod[*readParams, *readResult](p.ctx, p.cs, MethodRead, &readParams{ID: p.id, FD: 2, Max: p.maxRead})
		if err != nil {
			return
		}
		if len(r.Data) > 0 {
			p.stream.Write(r.Data)
		}
		if r.EOF {
			return
		}
	}
}

func (p *remoteProcess) Wait() (int, error) {
	<-p.waited
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ending != nil {
		return p.status, p.ending
	}
	return p.status, p.err
}

func (p *remoteProcess) Signal(sig os.Signal) error {
	var name string
	for n, s := range signals {
		if sig == s {
			name = n
		}
	}
	if name == "" {
		return fmt.Errorf("executor: cannot send %v", sig)
	}
	ctx, cancel := context.WithTimeout(p.ctx, processTimeout)
	defer cancel()
	r, err := sdk.CallCustomMethod[*signalParams, *signalResult](ctx, p.cs, MethodSignal, &signalParams{ID: p.id, Signal: name})
	if err != nil {
		return fmt.Errorf("executor: %w", err)
	}
	if r.Done {
		return os.ErrProcessDone
	}
	return nil
}

// Close ends the process in the executor, which closes its input,
// gives it a moment, then terminates and kills it, and returns once it
// has exited there and its standard error is written to the stream;
// then the reads and the wait still in flight end.
func (p *remoteProcess) Close() error {
	p.closeOne.Do(func() {
		p.mu.Lock()
		if p.stopWatch != nil {
			p.stopWatch()
		}
		p.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(p.ctx), processTimeout)
		r, err := sdk.CallCustomMethod[*idParams, *waitResult](ctx, p.cs, MethodClose, &idParams{ID: p.id})
		cancel()
		if err != nil {
			p.closeErr = fmt.Errorf("executor: %w", err)
			p.cancel()
		}
		p.finish(r, err)
		// The pump ends once the executor has closed the process; what
		// it had not read yet came with the reply, after what it read.
		<-p.pumped
		if err == nil && len(r.Stderr) > 0 && p.stream != nil {
			p.stream.Write(r.Stderr)
		}
		p.cancel()
	})
	return p.closeErr
}

// remoteStdin is the process's standard input: each Write is requests
// of at most maxWrite bytes, in order.
type remoteStdin struct {
	p      *remoteProcess
	mu     sync.Mutex
	closed bool
}

func (w *remoteStdin) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, os.ErrClosed
	}
	n := 0
	for len(b) > 0 {
		k := min(len(b), w.p.maxWrite)
		if _, err := sdk.CallCustomMethod[*writeParams, *emptyResult](w.p.ctx, w.p.cs, MethodWrite, &writeParams{ID: w.p.id, Data: b[:k]}); err != nil {
			return n, fmt.Errorf("executor: %w", err)
		}
		n += k
		b = b[k:]
	}
	return n, nil
}

// Close is end of file for the process.
func (w *remoteStdin) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	ctx, cancel := context.WithTimeout(w.p.ctx, processTimeout)
	defer cancel()
	if _, err := sdk.CallCustomMethod[*idParams, *emptyResult](ctx, w.p.cs, MethodCloseStdin, &idParams{ID: w.p.id}); err != nil {
		return fmt.Errorf("executor: %w", err)
	}
	return nil
}

// remoteStdout is the process's standard output: each Read is a
// request that waits for output.
type remoteStdout struct {
	p      *remoteProcess
	mu     sync.Mutex // one read at a time
	eof    bool
	closed atomic.Bool
}

func (r *remoteStdout) Read(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		switch {
		case r.closed.Load():
			return 0, os.ErrClosed
		case r.eof:
			return 0, io.EOF
		case len(b) == 0:
			return 0, nil
		}
		res, err := sdk.CallCustomMethod[*readParams, *readResult](r.p.ctx, r.p.cs, MethodRead, &readParams{ID: r.p.id, FD: 1, Max: min(len(b), r.p.maxRead)})
		if err != nil {
			if r.p.ctx.Err() != nil {
				return 0, os.ErrClosed
			}
			return 0, fmt.Errorf("executor: %w", err)
		}
		if len(res.Data) > len(b) {
			return 0, fmt.Errorf("executor: a read of %d bytes gave %d", len(b), len(res.Data))
		}
		r.eof = res.EOF
		if n := copy(b, res.Data); n > 0 {
			return n, nil
		}
	}
}

// Close stops later reads; one in flight ends with the process.
func (r *remoteStdout) Close() error {
	r.closed.Store(true)
	return nil
}
