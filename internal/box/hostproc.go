package box

// This file is the box host's own processes: the servers of a session's
// MCP units, started by an agentworkspace/process.start of ["dax-unit",
// NAME] and answered here with the semantics agentworkspace's
// mcpworkspace gives the executor's (the methods, their parameters and
// results, the bounds: package wire), under ids the executor's cannot
// be.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agentworkspace/mcpworkspace/wire"
)

// The process methods, as mcpworkspace names them.
const (
	methodPrefix     = wire.MethodPrefix
	methodStart      = wire.MethodStart
	methodWrite      = wire.MethodWrite
	methodRead       = wire.MethodRead
	methodCloseStdin = wire.MethodCloseStdin
	methodSignal     = wire.MethodSignal
	methodWait       = wire.MethodWait
	methodClose      = wire.MethodClose
)

// The bounds, as the executor's: processes open at once, bytes in one
// write and in one read's reply, and what is held of output no read
// took yet.
const (
	hostMaxProcesses  = wire.MaxProcesses
	hostMaxWriteBytes = wire.MaxWriteBytes
	hostMaxReadBytes  = wire.MaxReadBytes
	hostOutputBytes   = 64 << 10
)

// hostIDPrefix starts every id the box host gives a process; the
// executor's are p and a number.
const hostIDPrefix = "unit-"

// hostProcs is one session's unit servers.
type hostProcs struct {
	start func(ctx context.Context, unit string, stderr bool) (Proc, error)

	ctx    context.Context // ended with the session
	cancel context.CancelFunc

	mu     sync.Mutex
	next   int
	closed bool
	procs  map[string]*hostProc

	closeOne sync.Once // every close waits for the first to be done
}

type hostProc struct {
	p       Proc
	writeMu sync.Mutex
	stdout  *outBuf
	stderr  *outBuf // nil when not asked for
	gone    chan struct{}
	goneOne sync.Once

	exited chan struct{}
	status int
	err    error
	ended  string // wire.EndedTimeout or wire.EndedCanceled, once one ended it
	endMu  sync.Mutex
}

func newHostProcs(start func(context.Context, string, bool) (Proc, error)) *hostProcs {
	ctx, cancel := context.WithCancel(context.Background())
	return &hostProcs{start: start, ctx: ctx, cancel: cancel, procs: map[string]*hostProc{}}
}

// close ends every process, as the session's end does, and refuses
// another; it returns once they have ended, however often it is called.
func (t *hostProcs) close() { t.closeOne.Do(t.end) }

func (t *hostProcs) end() {
	t.mu.Lock()
	t.closed = true
	procs := make([]*hostProc, 0, len(t.procs))
	for _, hp := range t.procs {
		procs = append(procs, hp)
	}
	clear(t.procs)
	t.mu.Unlock()
	var wg sync.WaitGroup
	for _, hp := range procs {
		hp.setEnded(wire.EndedCanceled)
		wg.Go(hp.close)
	}
	wg.Wait()
	t.cancel()
}

func (hp *hostProc) setEnded(why string) {
	hp.endMu.Lock()
	if hp.ended == "" {
		hp.ended = why
	}
	hp.endMu.Unlock()
}

func (hp *hostProc) close() {
	hp.goneOne.Do(func() { close(hp.gone) })
	hp.p.Close()
	<-hp.exited
}

func (hp *hostProc) result() map[string]any {
	r := map[string]any{"status": hp.status}
	hp.endMu.Lock()
	ended := hp.ended
	hp.endMu.Unlock()
	switch {
	case ended != "":
		r["ended"] = ended
	case hp.err != nil:
		r["error"] = hp.err.Error()
	}
	return r
}

// The methods' parameters, as the executor takes them.
type (
	hostStartParams struct {
		Args      []string `json:"args"`
		TimeoutMs int64    `json:"timeoutMs,omitempty"`
		Stderr    bool     `json:"stderr,omitempty"`
	}
	hostIDParams struct {
		ID string `json:"id"`
	}
	hostWriteParams struct {
		ID   string `json:"id"`
		Data []byte `json:"data"`
	}
	hostReadParams struct {
		ID  string `json:"id"`
		FD  int    `json:"fd"`
		Max int    `json:"max,omitempty"`
	}
	hostSignalParams struct {
		ID     string `json:"id"`
		Signal string `json:"signal"`
	}
)

// serve answers one process method for a host-owned process (or the
// start of one) and gives its result, or an error.
func (t *hostProcs) serve(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case methodStart:
		var p hostStartParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return t.startProc(p)
	}
	var id hostIDParams
	if err := json.Unmarshal(params, &id); err != nil {
		return nil, err
	}
	hp, err := t.lookup(id.ID)
	if err != nil && method != methodClose {
		return nil, err
	}
	switch method {
	case methodWrite:
		var p hostWriteParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		if len(p.Data) > hostMaxWriteBytes {
			return nil, fmt.Errorf("a write of %d bytes; at most %d", len(p.Data), hostMaxWriteBytes)
		}
		hp.writeMu.Lock()
		defer hp.writeMu.Unlock()
		if _, err := hp.p.Stdin().Write(p.Data); err != nil {
			return nil, err
		}
		return struct{}{}, nil
	case methodRead:
		var p hostReadParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		var b *outBuf
		switch p.FD {
		case 1:
			b = hp.stdout
		case 2:
			if b = hp.stderr; b == nil {
				return nil, errors.New("standard error was not asked for at the start")
			}
		default:
			return nil, fmt.Errorf("fd %d: want 1 or 2", p.FD)
		}
		n := p.Max
		if n <= 0 || n > hostMaxReadBytes {
			n = hostMaxReadBytes
		}
		data, eof, err := b.take(ctx, n, hp.gone)
		if err != nil {
			return nil, err
		}
		r := map[string]any{}
		if len(data) > 0 {
			r["data"] = data
		}
		if eof {
			r["eof"] = true
		}
		return r, nil
	case methodCloseStdin:
		hp.writeMu.Lock()
		defer hp.writeMu.Unlock()
		hp.p.Stdin().Close()
		return struct{}{}, nil
	case methodSignal:
		var p hostSignalParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		done, err := hp.p.Signal(p.Signal)
		if err != nil {
			return nil, err
		}
		if done {
			return map[string]any{"done": true}, nil
		}
		return struct{}{}, nil
	case methodWait:
		select {
		case <-hp.exited:
			return hp.result(), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	case methodClose:
		t.mu.Lock()
		hp = t.procs[id.ID]
		delete(t.procs, id.ID)
		t.mu.Unlock()
		if hp == nil {
			return nil, fmt.Errorf("process %q: no such process in this session", id.ID)
		}
		hp.close()
		r := hp.result()
		if hp.stderr != nil {
			if rest := hp.stderr.rest(); len(rest) > 0 {
				r["stderr"] = rest
			}
		}
		return r, nil
	}
	return nil, fmt.Errorf("method %q: not a process method", method)
}

func (t *hostProcs) lookup(id string) (*hostProc, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if hp := t.procs[id]; hp != nil {
		return hp, nil
	}
	return nil, fmt.Errorf("process %q: no such process in this session", id)
}

func (t *hostProcs) startProc(p hostStartParams) (any, error) {
	if len(p.Args) != 2 || p.Args[0] != UnitCommand {
		return nil, fmt.Errorf("%s takes one unit's name", UnitCommand)
	}
	if p.TimeoutMs < 0 {
		return nil, errors.New("a negative timeout")
	}
	t.mu.Lock()
	switch {
	case t.closed:
		t.mu.Unlock()
		return nil, errors.New("the session is ending")
	case len(t.procs) >= hostMaxProcesses:
		t.mu.Unlock()
		return nil, fmt.Errorf("%d unit processes are open in this session, the most there may be; close one first", hostMaxProcesses)
	}
	t.mu.Unlock()
	proc, err := t.start(t.ctx, p.Args[1], p.Stderr)
	if err != nil {
		return nil, err
	}
	hp := &hostProc{p: proc, stdout: newOutBuf(), gone: make(chan struct{}), exited: make(chan struct{})}
	go hp.stdout.fill(proc.Stdout(), hp.gone)
	var stderrDone chan struct{}
	if r := proc.Stderr(); r != nil && p.Stderr {
		hp.stderr = newOutBuf()
		stderrDone = make(chan struct{})
		go func() {
			defer close(stderrDone)
			io.Copy(hp.stderr, r)
		}()
	}
	go func() {
		if stderrDone != nil {
			<-stderrDone
			hp.stderr.end()
		}
		status, err := proc.Wait()
		hp.status, hp.err = status, err
		close(hp.exited)
	}()
	if p.TimeoutMs > 0 {
		timer := time.AfterFunc(time.Duration(p.TimeoutMs)*time.Millisecond, func() {
			hp.setEnded(wire.EndedTimeout)
			hp.close()
		})
		go func() {
			<-hp.exited
			timer.Stop()
		}()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		go hp.close()
		return nil, errors.New("the session is ending")
	}
	t.next++
	id := fmt.Sprintf("%s%d", hostIDPrefix, t.next)
	t.procs[id] = hp
	return map[string]any{"id": id}, nil
}

// isHostID reports whether a process id is the box host's.
func isHostID(id string) bool { return strings.HasPrefix(id, hostIDPrefix) }

// outBuf is what a process wrote to one pipe that no read took yet, at
// most hostOutputBytes: standard output waits for room (fill), standard
// error loses what does not fit (Write), as the executor's.
type outBuf struct {
	mu      sync.Mutex
	buf     []byte
	eof     bool
	changed chan struct{}
}

func newOutBuf() *outBuf { return &outBuf{changed: make(chan struct{})} }

func (b *outBuf) notify() {
	close(b.changed)
	b.changed = make(chan struct{})
}

func (b *outBuf) fill(r io.Reader, gone <-chan struct{}) {
	defer b.end()
	tmp := make([]byte, 32<<10)
	for {
		b.mu.Lock()
		for len(b.buf) >= hostOutputBytes {
			ch := b.changed
			b.mu.Unlock()
			select {
			case <-ch:
			case <-gone:
				return
			}
			b.mu.Lock()
		}
		room := hostOutputBytes - len(b.buf)
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

func (b *outBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := hostOutputBytes - len(b.buf); room > 0 {
		b.buf = append(b.buf, p[:min(room, len(p))]...)
		b.notify()
	}
	return len(p), nil
}

func (b *outBuf) end() {
	b.mu.Lock()
	b.eof = true
	b.notify()
	b.mu.Unlock()
}

func (b *outBuf) rest() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.buf
	b.buf = nil
	b.notify()
	return out
}

func (b *outBuf) take(ctx context.Context, n int, gone <-chan struct{}) ([]byte, bool, error) {
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
