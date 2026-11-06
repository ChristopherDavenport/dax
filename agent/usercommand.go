package agent

// This file is a command the user runs themselves (a "!" line): in the
// session's workspace, as bash would, but not through the policy, since
// the person typed it; its output goes to the model as theirs.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agentturn"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/openresponses"
)

// userCommandNS is the namespace of the record's entry for a command
// the user ran: the command, its exit status and what was cut, beside
// the message the model is sent, so the record says the user ran it
// rather than that they typed its output.
const userCommandNS = "dax.user_command"

// userCommandLead opens the message a user's command is sent as, and
// marks it as output rather than a request (pinPrompts).
const userCommandLead = "I ran this command in the workspace myself:\n$ "

// maxUserCommandBytes is how much of a user's command's output the model
// is sent, as much as bash's; userCommandTimeout how long it may run.
const (
	maxUserCommandBytes = 50 << 10
	userCommandTimeout  = 10 * time.Minute
)

// UserCommand is a command the user ran and how it ended. Output is
// what the model is sent of what it printed.
type UserCommand struct {
	Command  string `json:"command"`
	ExitCode int    `json:"exit_code"`
	TimedOut bool   `json:"timed_out,omitempty"`
	// Cut is how many bytes of its output were left out.
	Cut    int    `json:"bytes_cut,omitempty"`
	Output string `json:"-"`
}

// RunUserCommand runs line with bash in the session's workspace, with
// the environment its commands get (credentials removed), not through
// the policy: the person at this session's own front typed it. Its
// line, output (at most 50 KiB, a URL's user:token@ replaced) and exit
// status are queued as the user's message, taken with the next prompt,
// or as a follow-up when a run goes; the record holds a
// dax.user_command entry beside it. A front over a wire must not offer
// it: a token holder there is not the person here.
func (s *Session) RunUserCommand(ctx context.Context, line string) (UserCommand, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return UserCommand{}, errors.New("no command")
	}
	out := &cappedOutput{max: maxUserCommandBytes}
	res, err := s.ws.Exec(ctx, workspace.Command{Args: []string{"bash", "-c", line}, Stream: out, Timeout: userCommandTimeout})
	c := UserCommand{Command: line}
	switch {
	case errors.Is(err, workspace.ErrCutShort):
		// It exited and left a job holding its output: the output so
		// far and its own status came beside the error.
		if res != nil {
			c.ExitCode = res.ExitCode
		}
	case err != nil:
		return UserCommand{}, err
	default:
		c.ExitCode, c.TimedOut = res.ExitCode, res.TimedOut
	}
	c.Output, c.Cut = out.text()
	var b strings.Builder
	b.WriteString(userCommandLead + line + "\n")
	b.WriteString(c.Output)
	if c.Output != "" && !strings.HasSuffix(c.Output, "\n") {
		b.WriteByte('\n')
	}
	if c.Cut > 0 {
		fmt.Fprintf(&b, "... [%d bytes cut]\n", c.Cut)
	}
	if c.TimedOut {
		fmt.Fprintf(&b, "[killed after %s]", userCommandTimeout)
	} else {
		fmt.Fprintf(&b, "[exit %d]", c.ExitCode)
	}
	if rec := s.Kit.Recorder(); rec != nil {
		if _, err := rec.Annotate(ctx, userCommandNS, c); err != nil {
			return UserCommand{}, fmt.Errorf("recording the command: %w", err)
		}
	}
	mode := agentturn.QueueSteer
	if s.State().Running {
		mode = agentturn.QueueFollowUp
	}
	if err := s.Queue(ctx, mode, openresponses.UserText(b.String())); err != nil {
		return UserCommand{}, err
	}
	return c, nil
}

// cappedOutput keeps the first max bytes written to it and counts the
// rest.
type cappedOutput struct {
	mu      sync.Mutex
	max     int
	buf     bytes.Buffer
	dropped int
}

func (c *cappedOutput) Write(p []byte) (int, error) {
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

// text is what was kept, a URL's user:password@ or token@ replaced, and
// how many bytes were not.
func (c *cappedOutput) text() (string, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return userinfo.ReplaceAllString(c.buf.String(), "${1}***@"), c.dropped
}

var userinfo = regexp.MustCompile(`(://)[^/@\s]+@`)
