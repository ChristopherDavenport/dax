package box

// This file is a unit's server as a process on the box host: `docker
// exec -i` into the unit's container, which the relay answers the
// session's process methods for.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agentworkspace/mcpworkspace/wire"
)

// Proc is a process the box host started for the session: a unit's
// server in its container.
type Proc interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	// Stderr is its standard error, nil when it was not asked for.
	Stderr() io.Reader
	// Signal sends INT, TERM or KILL; done says it had exited already.
	Signal(name string) (done bool, err error)
	// Wait waits for it to exit: its status, and an error when it could
	// not say one.
	Wait() (int, error)
	// Close ends it, as the executor ends its processes: its input
	// closed, a moment to go, then TERM, then KILL; it returns once it
	// has exited.
	Close() error
}

// closeGrace is how long Close waits after each step for the process
// to go. A var for the tests.
var closeGrace = 2 * time.Second

// pidPrefix marks the first line the wrapper writes, the process's id
// in its container, which Signal kills by.
const pidPrefix = "dax-unit-pid "

// wrapperName is the wrapper's $0, the name its shell goes by in the
// unit's container.
const wrapperName = "dax-unit"

// StartProc starts command in the MCP unit's container, with standard
// error kept when stderr: `docker exec -i <container> sh -c 'echo
// pid; exec "$@"' dax-unit <command>`, so that its id in the container,
// the first line it writes, is known to signal it by. It needs sh in
// the unit's image.
func (c *Client) StartProc(ctx context.Context, container string, command []string, stderr bool) (Proc, error) {
	if len(command) == 0 {
		return nil, errors.New("no command")
	}
	docker := c.DockerPath
	if docker == "" {
		docker = "docker"
	}
	args := append([]string{"exec", "-i", container, "sh", "-c", `echo "` + pidPrefix + `$$"; exec "$@"`, wrapperName}, command...)
	cmd := exec.Command(docker, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// Pipes of our own, not cmd.StdoutPipe and StderrPipe: Wait, which
	// runs as soon as the process starts, closes those when it exits,
	// and what the process wrote last is lost unread. These end only
	// when every writer has, so the reader gets all of it, then EOF;
	// Close closes the read ends.
	out, outW, err := os.Pipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	cmd.Stdout = outW
	p := &unitProc{docker: docker, container: container, cmd: cmd, stdin: stdin, exited: make(chan struct{}), readEnds: []*os.File{out}}
	var errPipe, errW *os.File
	if stderr {
		if errPipe, errW, err = os.Pipe(); err != nil {
			stdin.Close()
			out.Close()
			outW.Close()
			return nil, err
		}
		cmd.Stderr = errW
		p.readEnds = append(p.readEnds, errPipe)
	} else {
		// Kept, a little, for the error when it does not start.
		cmd.Stderr = &p.early
	}
	err = cmd.Start()
	// The child has its copies of the write ends; ours would keep the
	// pipes open after it exits.
	outW.Close()
	if errW != nil {
		errW.Close()
	}
	if err != nil {
		stdin.Close()
		p.closeReadEnds()
		return nil, err
	}
	p.stdout = bufio.NewReaderSize(out, 32<<10)
	go func() {
		err := cmd.Wait()
		p.status = -1
		if cmd.ProcessState != nil {
			p.status = cmd.ProcessState.ExitCode()
		}
		var ee *exec.ExitError
		if err != nil && !errors.As(err, &ee) {
			p.err = err
		}
		close(p.exited)
	}()
	if errPipe != nil {
		p.stderr = errPipe
	}
	// The first line is the wrapper's; the process's own output follows.
	type first struct {
		line string
		err  error
	}
	got := make(chan first, 1)
	go func() {
		line, err := p.stdout.ReadString('\n')
		got <- first{line, err}
	}()
	select {
	case f := <-got:
		pid, ok := strings.CutPrefix(strings.TrimSpace(f.line), strings.TrimSpace(pidPrefix))
		n, err := strconv.Atoi(strings.TrimSpace(pid))
		if f.err != nil || !ok || err != nil || n <= 0 {
			p.Close()
			return nil, fmt.Errorf("%s: it did not start (%s)", container, strings.TrimSpace(p.early.String()))
		}
		p.pid = n
	case <-ctx.Done():
		p.Close()
		return nil, ctx.Err()
	}
	return p, nil
}

type unitProc struct {
	docker, container string
	cmd               *exec.Cmd
	stdin             io.WriteCloser
	stdout            *bufio.Reader
	stderr            io.Reader
	early             limitedBuffer
	pid               int

	exited chan struct{}
	status int
	err    error

	readEnds []*os.File // the output pipes' read ends, closed by Close
	closeOne sync.Once
}

func (p *unitProc) closeReadEnds() {
	for _, f := range p.readEnds {
		f.Close()
	}
}

func (p *unitProc) Stdin() io.WriteCloser { return p.stdin }
func (p *unitProc) Stdout() io.Reader     { return p.stdout }
func (p *unitProc) Stderr() io.Reader {
	if p.stderr == nil {
		return nil
	}
	return p.stderr
}

func (p *unitProc) Wait() (int, error) {
	<-p.exited
	return p.status, p.err
}

func (p *unitProc) gone() bool {
	select {
	case <-p.exited:
		return true
	default:
		return false
	}
}

func (p *unitProc) Signal(name string) (bool, error) {
	// The names are the wire's, which kill(1) takes as they are.
	if _, ok := wire.SignalOf(name); !ok {
		return false, fmt.Errorf("signal %q: want INT, TERM or KILL", name)
	}
	if p.gone() || p.pid == 0 {
		return true, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	kill := exec.CommandContext(ctx, p.docker, "exec", p.container, "sh", "-c", "kill -"+name+" "+strconv.Itoa(p.pid))
	if err := kill.Run(); err != nil {
		if p.gone() {
			return true, nil
		}
		return false, fmt.Errorf("signal %s: %w", name, err)
	}
	return false, nil
}

// Close ends the process: its input closed, then TERM, then KILL, each
// given closeGrace; then the docker command itself.
func (p *unitProc) Close() error {
	p.closeOne.Do(func() {
		defer p.closeReadEnds()
		p.stdin.Close()
		for _, sig := range []string{"", wire.SignalTERM, wire.SignalKILL} {
			if sig != "" {
				p.Signal(sig)
			}
			select {
			case <-p.exited:
				return
			case <-time.After(closeGrace):
			}
		}
		if p.cmd.Process != nil {
			p.cmd.Process.Kill()
		}
		<-p.exited
	})
	return nil
}

// limitedBuffer keeps the first 4 KiB written to it.
type limitedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := 4096 - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
