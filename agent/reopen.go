package agent

// Another session from inside one: /new opens a fresh conversation and
// /resume an earlier one, each over the settings this session runs
// with now rather than those it was opened with (#13, #164). It is a
// new session, not a reset of this one: one session is one
// conversation, and the record of this one stays as it is.

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// addedMCP is a server AddMCP started: the label RemoveMCP takes, and
// the server, which Reopen starts again.
type addedMCP struct {
	label  string
	server MCPServer
}

// Reopen opens another session over this one's live settings: a new
// one for id "", or the recorded session id, resumed. What the person
// changed during this session carries over: the model (/model),
// reasoning (/think), the permission mode (/mode, Shift-Tab) and the
// MCP servers /mcp add started, which start again in the new session,
// each with a label of its own. What is this conversation's alone does
// not: the session grants ("yes, always" for this session), the ports
// /port forwarded and the calls a run left held, which stay with this
// session's record. The new session hangs from the universal root, as
// any does, or goes on from the resumed one's head.
//
// The two share the store: the new session closes it, and this one,
// which the caller closes once it has moved to the new one, no longer
// does. A run must have ended first. Resuming needs a store, and is
// refused for this session's own ID.
func (s *Session) Reopen(ctx context.Context, id string) (*Session, error) {
	if s.ag.State().Running {
		return nil, errors.New("waits for the run to end")
	}
	if id != "" {
		if !s.recorded {
			return nil, errors.New("resume needs a session store")
		}
		if id == s.ID() {
			return nil, fmt.Errorf("session %s is this one", id)
		}
	}
	o := s.base
	think, model := s.now()
	o.Think = think
	if o.Policy != nil && s.modes.settings != nil {
		mode, _ := s.Mode()
		p := *o.Policy
		p.Mode = mode
		o.Policy = &p
	}
	s.serversMu.Lock()
	added := append([]addedMCP{}, s.added...)
	s.serversMu.Unlock()

	store, own := s.store, s.own
	if !s.recorded {
		store, own = memoryStore(), true
	}
	n, err := open(ctx, o, store, own, id)
	if err != nil {
		if c, ok := store.(io.Closer); ok && !s.recorded {
			c.Close()
		}
		return nil, err
	}
	n.recorded = s.recorded
	// A failure from here closes the new session, which must leave the
	// store this one still uses open.
	fail := func(err error) (*Session, error) {
		if s.recorded {
			n.own = false
		}
		n.Close()
		return nil, err
	}
	if id != "" {
		if err := n.resumed(id); err != nil {
			return fail(err)
		}
	}
	// The model by SetModel, as /model set it here: a price the meter
	// asks for at open is the starting model's.
	if model != o.Model {
		if err := n.SetModel(model); err != nil {
			return fail(err)
		}
	}
	for _, a := range added {
		if _, err := n.AddMCP(ctx, a.server.Name, a.server.Command); err != nil {
			return fail(fmt.Errorf("mcp %s: %w", a.server.Name, err))
		}
	}
	if s.recorded {
		s.own = false
	}
	return n, nil
}
