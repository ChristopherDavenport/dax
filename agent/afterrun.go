package agent

// The extensions' AfterRun hooks: what each run of the person's main
// agent did, gathered from its events, handed to them once it ends.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"

	"github.com/ChristopherDavenport/dax/extension"
)

// afterRuns are the extensions' AfterRun hooks and the runs in flight
// they will be told about.
type afterRuns struct {
	hooks []func(context.Context, extension.Run)
	// lookup finds a tool by name, for its annotations.
	lookup func(string) (agenttool.Tool, bool)

	mu   sync.Mutex
	runs map[string]*extension.Run
	// ctx ends when the session closes; wg is the hooks still running,
	// which the close waits for.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	// closed is set once the session closes: no hook starts after.
	closed bool
}

// afterRunHooks builds the extensions' AfterRun hooks over env.
func afterRunHooks(exts []extension.Extension, env extension.Env) ([]func(context.Context, extension.Run), error) {
	var out []func(context.Context, extension.Run)
	for _, e := range exts {
		if e.AfterRun == nil {
			continue
		}
		h, err := e.AfterRun(env)
		if err != nil {
			return nil, fmt.Errorf("extension %s: after run: %w", e.Name, err)
		}
		if h != nil {
			out = append(out, h)
		}
	}
	return out, nil
}

// openWorld reports whether a call of the tool name reaches past the
// workspace and the session: an MCP server's, or one annotated so.
func (r *afterRuns) openWorld(name string) bool {
	if strings.HasPrefix(strings.ToLower(name), mcpPrefixLower) {
		return true
	}
	if r.lookup == nil {
		return false
	}
	t, ok := r.lookup(name)
	return ok && agenttool.AnnotationsOf(t).OpenWorld
}

// handle follows the main agent's events: a run's calls and usage as
// they come, and at its end the hooks, each in a goroutine of its own
// so the loop goes on.
func (r *afterRuns) handle(_ context.Context, ev agentturn.Event) error {
	if len(r.hooks) == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch e := ev.(type) {
	case *agentturn.RunStart:
		if r.runs == nil {
			r.runs = map[string]*extension.Run{}
		}
		r.runs[e.RunID] = &extension.Run{ID: e.RunID}
	case *agentturn.ToolStart:
		if run := r.runs[e.RunID]; run != nil {
			run.Calls = append(run.Calls, e.Name)
			if r.openWorld(e.Name) {
				run.OpenWorld = true
			}
		}
	case *agentturn.ResponseEnd:
		if run := r.runs[e.RunID]; run != nil && e.Response != nil && e.Response.Usage != nil {
			run.OutputTokens += e.Response.Usage.OutputTokens
		}
	case *agentturn.RunEnd:
		run := r.runs[e.RunID]
		delete(r.runs, e.RunID)
		if run == nil {
			// A run refused before it started: nothing ran.
			return nil
		}
		run.End = e
		if r.ctx == nil || r.closed {
			return nil
		}
		done := *run
		for _, h := range r.hooks {
			r.wg.Add(1)
			go func() {
				defer r.wg.Done()
				h(r.ctx, done)
			}()
		}
	}
	return nil
}

// afterRunGrace is how long a closing session lets its hooks finish
// before it ends their context: a pass after -p's one run is short, and
// should land.
const afterRunGrace = 30 * time.Second

// stop waits for the hooks running, at most afterRunGrace, then ends
// their context and waits for them.
func (r *afterRuns) stop() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	finished := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(afterRunGrace):
	}
	if r.cancel != nil {
		r.cancel()
	}
	<-finished
}
