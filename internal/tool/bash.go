package tool

import (
	"bytes"
	"context"
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

			cmd := command(ctx, &Analyzer{Dir: dir, MaxFile: cfg.maxFile}, in.Command, base)
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
// A command line the policy would auto-allow (Analyzer.Auto: the safe
// subset, read-only commands, a git configuration that names no
// program) runs as the plan it parsed to, rendered with every word
// quoted and --no-ext-diff --no-textconv added to git diff, log and
// show, in an environment with AutoEnv added. Everything else, every
// command a person approved, is bash -c exactly as given, in the
// environment the user has minus credentials: their hooks, their
// sshCommand and their GIT_CONFIG_* are theirs.
func command(ctx context.Context, an *Analyzer, command string, base []string) *exec.Cmd {
	if c := an.Check(ctx, command); c.Auto {
		cmd := exec.CommandContext(ctx, "bash", "-c", c.Render())
		cmd.Env = append(append([]string(nil), base...), AutoEnv()...)
		return cmd
	}
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Env = append([]string(nil), base...)
	return cmd
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
