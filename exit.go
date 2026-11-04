package dax

// This file is how -p tells its caller the way its run ended: an exit
// status per end reason, -timeout, and -result's JSON.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agentturn"
)

// Exit statuses of -p by how its run ended. 0 is done and 1 an error,
// as for any command; the rest are a run that ended without being done.
const (
	exitStopped       = 3
	exitInputRequired = 4
	exitAborted       = 5
)

// exitStatus is the status -p exits with after a run that ended so.
func exitStatus(end *agentturn.RunEnd) int {
	switch end.Reason {
	case agentturn.ReasonDone:
		return 0
	case agentturn.ReasonStopped:
		return exitStopped
	case agentturn.ReasonInputRequired:
		return exitInputRequired
	case agentturn.ReasonAborted:
		return exitAborted
	}
	return 1
}

// runEnded is a -p run that ended other than done, whose end line is
// already printed: Main exits with its status and prints nothing more.
type runEnded struct {
	status int
	reason agentturn.Reason
}

func (e *runEnded) Error() string { return "the run ended " + string(e.reason) }

// timeoutError is -timeout's cause: the run's context ends with it, so
// the run's end, and the record, say the run was aborted at the
// deadline rather than "context canceled".
type timeoutError struct{ d time.Duration }

func (e *timeoutError) Error() string { return fmt.Sprintf("timeout: -timeout %s reached", e.d) }

// runUsage sums the main agent's model calls and their tokens, from the
// run's events.
type runUsage struct {
	mu sync.Mutex
	u  resultUsage
}

func (r *runUsage) count(_ context.Context, ev agentturn.Event) error {
	if e, ok := ev.(*agentturn.ResponseEnd); ok {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.u.ModelCalls++
		if u := e.Response.Usage; u != nil {
			r.u.InputTokens += u.InputTokens
			r.u.CachedTokens += u.InputTokensDetails.CachedTokens
			r.u.OutputTokens += u.OutputTokens
		}
	}
	return nil
}

func (r *runUsage) total() resultUsage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.u
}

// runResult is what -result writes: the run's end as the record has it
// (reason, cause, error, the calls left pending), the session it is
// recorded in, the status -p would have exited with, and the main
// agent's usage (a sub-agent's calls are its own session's).
type runResult struct {
	Session string       `json:"session,omitempty"`
	Reason  string       `json:"reason"`
	Cause   string       `json:"cause,omitempty"`
	Error   string       `json:"error,omitempty"`
	Pending []resultCall `json:"pending"`
	Exit    int          `json:"exit"`
	Usage   resultUsage  `json:"usage"`
}

type resultCall struct {
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Why       string `json:"why"`
}

type resultUsage struct {
	ModelCalls   int `json:"model_calls"`
	InputTokens  int `json:"input_tokens"`
	CachedTokens int `json:"cached_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// writeResult writes -result's file, private to the user, as the rest
// of what dax writes about a session is.
func writeResult(path, session string, end *agentturn.RunEnd, usage resultUsage) error {
	r := runResult{Session: session, Reason: string(end.Reason), Cause: string(end.Cause), Pending: []resultCall{}, Exit: exitStatus(end), Usage: usage}
	if end.Err != nil {
		r.Error = end.Err.Error()
	}
	for _, p := range end.Pending {
		if p.Call == nil {
			continue
		}
		r.Pending = append(r.Pending, resultCall{CallID: p.Call.CallID, Name: p.Call.Name, Arguments: p.Call.Arguments, Why: string(p.Reason)})
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("-result: %w", err)
	}
	return nil
}

// sessionIDOf is the ID of the session t records, "" when it says none.
func sessionIDOf(t agentturn.Control) string {
	if s, ok := t.(interface{ ID() string }); ok {
		return s.ID()
	}
	return ""
}
