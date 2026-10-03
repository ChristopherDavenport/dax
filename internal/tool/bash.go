package tool

import (
	"bytes"
	"context"
	"crypto/hmac"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ChristopherDavenport/agenttool"
)

const (
	defaultBashTimeout = 2 * time.Minute
	maxBashBytes       = 50 << 10
)

// BashArgs are the arguments of the bash tool.
type BashArgs struct {
	Command string `json:"command" desc:"The command to run"`
	Timeout int    `json:"timeout_seconds,omitempty" desc:"Kill the command after this many seconds (default 120)"`
	// Stamp is set by dex when the policy allowed the command without
	// asking; a value that dex did not set makes the call fail.
	Stamp string `json:"dex_stamp,omitempty" desc:"Set by dex; leave it out"`
}

// BashOption configures the bash tool.
type BashOption func(*bashConfig)

type bashConfig struct {
	env     []string
	maxFile int64
}

// WithMaxFile sets the largest file the auto-allowed cat, head, tail,
// wc and grep may be given; see Analyzer.
func WithMaxFile(n int64) BashOption { return func(c *bashConfig) { c.maxFile = n } }

// WithEnv sets the environment commands start with, before GitEnv is
// added. The default is the current one without its credentials; see
// ChildEnv.
func WithEnv(env []string) BashOption { return func(c *bashConfig) { c.env = env } }

// Bash returns a tool that runs a shell command in dir. It is
// sequential: a batch that contains a shell command runs one call at a
// time, so a command never races a concurrent edit of the same file.
func Bash(dir string, opts ...BashOption) agenttool.Tool {
	var cfg bashConfig
	for _, o := range opts {
		o(&cfg)
	}
	base := cfg.env
	if base == nil {
		base = DefaultEnv(nil)
	}
	return agenttool.New("bash", "Run a bash command in the working directory and return its combined output and exit code.",
		func(ctx context.Context, in BashArgs) (string, error) {
			if strings.TrimSpace(in.Command) == "" {
				return "", errors.New("command is required")
			}
			timeout := defaultBashTimeout
			if in.Timeout > 0 {
				timeout = time.Duration(in.Timeout) * time.Second
			}
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			cmd, err := command(ctx, &Analyzer{Dir: dir, MaxFile: cfg.maxFile}, in, base)
			if err != nil {
				return "", err
			}
			cmd.Dir = dir
			// Run in its own process group so cancellation reaches children too.
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
			cmd.WaitDelay = 2 * time.Second

			// Keep the first maxBashBytes of the output and count the
			// rest: a cat of a huge file or a runaway command costs
			// what its first screen does.
			out := &capWriter{max: maxBashBytes}
			cmd.Stdout = out
			cmd.Stderr = out
			runErr := cmd.Run()

			var b strings.Builder
			b.WriteString(out.String())
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
				b.WriteByte('\n')
			}
			switch {
			case errors.Is(ctx.Err(), context.DeadlineExceeded):
				fmt.Fprintf(&b, "[killed after %s]", timeout)
			case ctx.Err() != nil:
				return "", ctx.Err()
			case runErr == nil:
				b.WriteString("[exit 0]")
			default:
				var ee *exec.ExitError
				if errors.As(runErr, &ee) {
					fmt.Fprintf(&b, "[exit %d]", ee.ExitCode())
				} else {
					return "", runErr
				}
			}
			return b.String(), nil
		}, agenttool.WithSequential())
}

// command is what runs a bash call, and in which environment.
//
// A call the policy allowed without asking carries the stamp of the plan
// it approved (StampArgs). It runs only if the line still analyses to
// that plan: as the plan rendered, every word quoted and --no-ext-diff
// --no-textconv added to git diff, log and show, in an environment with
// AutoEnv added. If the line has changed since, because a file is
// different or the repository's git config now names a program, the
// call fails with ErrChanged, and the original line is never run
// instead. A call without a stamp, one a person approved or one run with
// no policy, is bash -c exactly as given, in the environment the user
// has minus credentials: their hooks, their sshCommand and their
// GIT_CONFIG_* are theirs.
func command(ctx context.Context, an *Analyzer, in BashArgs, base []string) (*exec.Cmd, error) {
	if in.Stamp != "" {
		c := an.Check(ctx, in.Command)
		if !c.Auto || !hmac.Equal([]byte(stampOf(c.Render())), []byte(in.Stamp)) {
			return nil, ErrChanged
		}
		cmd := exec.CommandContext(ctx, "bash", "-c", c.Render())
		cmd.Env = append(append([]string(nil), base...), AutoEnv()...)
		return cmd, nil
	}
	cmd := exec.CommandContext(ctx, "bash", "-c", in.Command)
	cmd.Env = append([]string(nil), base...)
	return cmd, nil
}

// capWriter keeps the first max bytes written to it and counts the
// rest, so the output of a command is bounded in memory.
type capWriter struct {
	mu      sync.Mutex
	max     int
	buf     bytes.Buffer
	dropped int
}

func (c *capWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	if room := c.max - c.buf.Len(); room > 0 {
		k := min(room, len(p))
		c.buf.Write(p[:k])
		p = p[k:]
	}
	c.dropped += len(p)
	return n, nil
}

func (c *capWriter) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.buf.String()
	if c.dropped > 0 {
		s += fmt.Sprintf("\n... [truncated, %d bytes omitted]", c.dropped)
	}
	return s
}
