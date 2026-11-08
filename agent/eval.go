package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/tool"
)

// A dax configuration run under an evaluation runner (agenteval's
// Runner) records into the recorder the runner opens, so the run's
// session is what a session of dax's own would be: HeaderOf for the
// runner's Header, SessionOptionsOf for its SessionOptions, and Build,
// with Options.Recorder set to the runner's recorder, for its Build:
//
//	agenteval.Runner{
//		Store:          store,
//		Header:         func(agenteval.Task) agentsession.Header { return agent.HeaderOf(o) },
//		SessionOptions: func(agenteval.Task) []session.Option { return agent.SessionOptionsOf(o) },
//		Build: func(ctx context.Context, _ agenteval.Task, rec *session.Recorder) (agentturn.Config, func() error, error) {
//			run := o
//			run.Root, run.Store, run.Recorder = "", nil, rec
//			return agent.Build(ctx, run)
//		},
//	}

// listTimeout bounds HeaderOf's listing of a servers workspace's
// servers, which the runner's Header gives no context for.
const listTimeout = 30 * time.Second

// HeaderOf is the header dax writes for a new session under o: the
// workspace's root as the cwd, dax's harness, media as sidecar blobs,
// and dax's own members (the workspace, an executor's hashed address, a
// box, the context strategy, a bare profile, the servers workspaces
// beside it, whose servers it lists). It is for a recorder dax does not
// open, an evaluation runner's; Build refuses a recorder whose header
// is not this one.
func HeaderOf(o Options) agentsession.Header {
	return headerUnder(context.Background(), o)
}

// headerUnder is HeaderOf, its listing of servers workspaces under ctx.
func headerUnder(ctx context.Context, o Options) agentsession.Header {
	o = o.named()
	ws, done, err := sessionWorkspace(o)
	if err != nil {
		// The header names the directory alone; Build fails on the
		// same error.
		return newHeader(o, o.Dir, nil)
	}
	defer done()
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	beside, _ := besideOf(ctx, o, ws)
	return newHeader(o, ws.Root(), extrasOf(o, ws.Descriptor(), beside))
}

// sessionWorkspace is the workspace a session under o acts in, as open
// chooses it, and what releases it.
func sessionWorkspace(o Options) (workspace.Workspace, func(), error) {
	switch {
	case o.Executor != nil:
		return o.Executor.Workspace(), func() {}, nil
	case o.Workspace != nil:
		return o.Workspace, func() {}, nil
	}
	local, err := workspace.NewLocal(o.Dir, tool.DefaultEnv(o.PassEnv, o.secrets()...))
	if err != nil {
		return nil, nil, fmt.Errorf("workspace: %w", err)
	}
	return local, func() { local.Close() }, nil
}

// besideOf is the servers workspaces beside ws as the header records
// them, each with the servers it offers.
func besideOf(ctx context.Context, o Options, ws workspace.Workspace) ([]ServersBeside, error) {
	named, err := serversWorkspaces(o)
	if err != nil {
		return nil, err
	}
	var out []ServersBeside
	for _, b := range named {
		sp, err := checkServersWorkspace(b.ws, ws)
		if err != nil {
			return nil, err
		}
		d := b.ws.Descriptor()
		list, err := offeredServers(ctx, sp, "the servers workspace "+b.name, PlacedServersWorkspace, where(d))
		if err != nil {
			return nil, err
		}
		rec := ServersBeside{Name: b.name, Workspace: workspace.Descriptor{Kind: d.Kind, Ref: d.Ref, Root: d.Root}, Servers: []string{}}
		for _, m := range list {
			rec.Servers = append(rec.Servers, m.Name)
		}
		out = append(out, rec)
	}
	return out, nil
}

// evalBound is the session Build opened over each recorder whose
// options SessionOptionsOf gave, by recorder, until Build takes it.
var evalBound sync.Map // *session.Recorder -> *evalSlot

// evalSlot is the session a recorder's env and parts functions read,
// set by Build once it has opened it.
type evalSlot struct {
	mu sync.Mutex
	s  *Session
}

func (e *evalSlot) session() *Session {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.s
}

// SessionOptionsOf are the options a recorder of a session under o is
// opened with, as dax opens its own: the environment entry from the
// workspace the tools act in, and the instructions as the kit's parts.
// Both read the session Build opens over that recorder, which the last
// option binds them to: o itself is only what Build is given.
func SessionOptionsOf(Options) []session.Option {
	slot := &evalSlot{}
	return []session.Option{
		session.WithEnv(func(ctx context.Context) (*agentsession.EnvEntry, error) {
			s := slot.session()
			if s == nil {
				return nil, nil
			}
			return envOf(s.x, nil)(ctx)
		}),
		session.WithInstructionsParts(func(ctx context.Context, req openresponses.Request) ([]agentsession.InstructionPart, []agentsession.OmittedPart) {
			s := slot.session()
			if s == nil || s.Kit == nil {
				return nil, nil
			}
			return s.Kit.PartsFor(ctx, req)
		}),
		func(r *session.Recorder) { evalBound.Store(r, slot) },
	}
}

// Build opens a session under o, which records into o.Recorder, and
// returns the configuration its agent runs, for a runner that runs the
// agent itself (agenteval's Runner.Build), and the session's close. The
// recorder's header must be HeaderOf's: one whose cwd or dax members
// differ is refused, so a run is never recorded as another
// configuration's, and refused before the session opens, so nothing is
// written into it. A recorder SessionOptionsOf's options opened is
// bound to the session, so its environment entries and instructions'
// parts are the session's.
//
// The configuration is the session's agent's: its policy, hooks and
// sub-agents. What the session's own agent does beside it (the fronts'
// control, a mode switched mid-run) is not the runner's.
func Build(ctx context.Context, o Options) (agentturn.Config, func() error, error) {
	if o.Recorder == nil {
		return agentturn.Config{}, nil, errors.New("build: no Recorder; use New for a session with a store")
	}
	// The binding is dropped on every way out; the session is set in it
	// once it opens.
	v, bound := evalBound.LoadAndDelete(o.Recorder)
	if err := sameHeader(ctx, o.Recorder, headerUnder(ctx, o)); err != nil {
		return agentturn.Config{}, nil, err
	}
	s, err := New(ctx, o)
	if err != nil {
		return agentturn.Config{}, nil, err
	}
	if bound {
		slot := v.(*evalSlot)
		slot.mu.Lock()
		slot.s = s
		slot.mu.Unlock()
	}
	return s.withModeTurn(s.withPinnedFacts(s.Kit.Config())), s.Close, nil
}

// sameHeader says whether rec's session has want, the header dax writes:
// its cwd, harness and every member of dax's own.
func sameHeader(ctx context.Context, rec *session.Recorder, want agentsession.Header) error {
	got, err := rec.Store().Open(ctx, rec.SessionID())
	if err != nil {
		return fmt.Errorf("build: the recorder's session: %w", err)
	}
	h := got.Header()
	var diff []string
	if h.CWD != want.CWD {
		diff = append(diff, fmt.Sprintf("cwd %q, not %q", h.CWD, want.CWD))
	}
	if h.Harness == nil || h.Harness.Name != want.Harness.Name || h.Harness.Version != want.Harness.Version {
		diff = append(diff, "the harness is not "+want.Harness.Name+" "+want.Harness.Version)
	}
	if h.Media != want.Media {
		diff = append(diff, fmt.Sprintf("media %q, not %q", h.Media, want.Media))
	}
	keys := slices.Sorted(maps.Keys(want.Extra))
	for k := range h.Extra {
		if strings.HasPrefix(k, "dax.") && !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		if !sameJSON(h.Extra[k], want.Extra[k]) {
			diff = append(diff, k+" differs")
		}
	}
	if diff != nil {
		return fmt.Errorf("build: the recorder's header is not the one dax writes (agent.HeaderOf): %s", strings.Join(diff, "; "))
	}
	return nil
}

// sameJSON says whether a and b are one JSON value, both absent
// included.
func sameJSON(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var ca, cb bytes.Buffer
	if json.Compact(&ca, a) != nil || json.Compact(&cb, b) != nil {
		return bytes.Equal(a, b)
	}
	return bytes.Equal(ca.Bytes(), cb.Bytes())
}
