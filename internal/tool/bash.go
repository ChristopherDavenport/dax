package tool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
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

// Bash returns a tool that runs a shell command in dir. It is
// sequential: a batch that contains a shell command runs one call at a
// time, so a command never races a concurrent edit of the same file.
func Bash(dir string) agenttool.Tool {
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

			cmd := exec.CommandContext(ctx, "bash", "-c", in.Command)
			cmd.Dir = dir
			// Run in its own process group so cancellation reaches children too.
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
			cmd.WaitDelay = 2 * time.Second

			var out bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &out
			runErr := cmd.Run()

			var b strings.Builder
			b.WriteString(truncate(out.String(), maxBashBytes))
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
