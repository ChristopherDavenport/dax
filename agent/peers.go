package agent

// This file is the session's side of A2A as a callee: a host that serves
// the session (dax serve -a2a) runs each A2A caller's conversation on
// an agent of its own under the session's configuration, and records
// it in a session of its own. One session is one conversation, so a
// caller's conversation is never written into the served one.

import (
	"context"
	"errors"
	"sync"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
)

// PeerConfig is the configuration an A2A caller's conversation runs
// under when the session's host serves A2A beside it (agentturn's
// front/a2a): the session's agent's, so the same model and reasoning,
// extensions, policy and workspace, read when it is called; its tools
// are the kit's, so an MCP server added or removed later is added or
// removed for the conversations too. A call the policy asks about, a
// sub-agent's asked call, or a tool's question, goes to the views
// attached to the session (Viewing), naming the caller's conversation
// (PeerRecord.Attach). With none attached it is not asked, and when the
// last one goes while it waits it is given up: the policy's question
// becomes a refusal, never an allow. Its ToolRecorder is nil;
// PeerRecord.Attach routes a run's records to the conversation's
// session.
//
// The conversations share the workspace with the session, and with each
// other, as parallel sub-agents do.
func (s *Session) PeerConfig() agentturn.Config {
	cfg := s.ag.Config()
	cfg.ToolRecorder = nil
	return cfg
}

// PeerRecord is the record of one A2A caller's conversation: a dax
// session of its own in the session's store, listed and resumed as any
// other.
type PeerRecord struct {
	s   *Session
	rec *session.Recorder
}

// OpenPeer starts the session that records an A2A caller's conversation,
// or with id resumes the one that does. The session names the harness
// and the workspace's root as the served session does, so -list in
// the project shows it.
func (s *Session) OpenPeer(ctx context.Context, id string) (*PeerRecord, error) {
	if !s.recorded {
		return nil, errors.New("a conversation is recorded in the session's store, and this session keeps none")
	}
	opts := []session.Option{session.WithInstructionsParts(s.Kit.PartsFor), s.envOption()}
	var (
		rec *session.Recorder
		err error
	)
	if id != "" {
		rec, _, err = session.Resume(ctx, s.store, id, opts...)
	} else {
		rec, _, err = session.Start(ctx, s.store, s.header(), opts...)
	}
	if err != nil {
		return nil, err
	}
	return &PeerRecord{s: s, rec: rec}, nil
}

// ID is the session's ID.
func (p *PeerRecord) ID() string { return p.rec.SessionID() }

// Attach records a's runs in the conversation's session: the tools'
// records, the run's events, and on the context it returns the
// recorder the session's hooks write their verdicts with, the session
// for the model calls and memory, and who the questions of the run come
// from, which a front is told with each (who: the caller, say). detach
// undoes it once the run is over, and revokes the skill grants the run
// made, as the session's own end at its next message, so the kit keeps
// nothing for a conversation between its tasks.
func (p *PeerRecord) Attach(ctx context.Context, a *agentturn.Agent, who string) (context.Context, func(), error) {
	cfg := a.Config()
	cfg.ToolRecorder = p.rec.RecordFunc()
	if err := a.SetConfig(cfg); err != nil {
		return nil, nil, err
	}
	unsubscribe := p.rec.Attach(a)
	id := p.rec.SessionID()
	ctx = agentkit.ContextWithRecorder(ctx, p.rec)
	ctx = session.ContextWithSessionID(ctx, id)
	ctx = agentmemory.WithSession(ctx, id)
	ctx = context.WithValue(ctx, peerKey{}, who+" (session "+id+")")
	grants := context.WithoutCancel(ctx)
	return ctx, func() {
		unsubscribe()
		p.s.Kit.RevokeSkillGrants(grants)
	}, nil
}

type peerKey struct{}

// forPeer puts the conversation a question comes from ahead of it,
// when it comes from an A2A caller's: the fronts answering are the
// served session's, and the call is not that session's.
func forPeer(ctx context.Context, msg string) string {
	if who, ok := ctx.Value(peerKey{}).(string); ok && who != "" {
		return "[" + who + "] " + msg
	}
	return msg
}

// views counts the views attached to a session; gone is closed when the
// last one goes.
type views struct {
	mu   sync.Mutex
	n    int
	gone chan struct{}
}

// Viewing says a view is attached to the session, one that sees its
// events and can answer its questions, until the returned func is
// called. A host serving the session counts each follower of its
// events. Answering says questions may wait for a view; Viewing says
// one is there, which an A2A caller's conversation needs: its questions
// are put only to a view attached when they are asked.
func (s *Session) Viewing() (done func()) {
	v := &s.views
	v.mu.Lock()
	if v.n == 0 {
		v.gone = make(chan struct{})
	}
	v.n++
	v.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			v.mu.Lock()
			defer v.mu.Unlock()
			if v.n--; v.n == 0 {
				close(v.gone)
			}
		})
	}
}

// peerAsk is the context a question is asked under: for an A2A caller's
// conversation, one that ends when the last view goes, and ok false
// when no view is attached; for any other, ctx. done ends it.
func (s *Session) peerAsk(ctx context.Context) (_ context.Context, done func(), ok bool) {
	if _, peer := ctx.Value(peerKey{}).(string); !peer {
		return ctx, func() {}, true
	}
	s.views.mu.Lock()
	n, gone := s.views.n, s.views.gone
	s.views.mu.Unlock()
	if n == 0 {
		return ctx, nil, false
	}
	ctx, cancel := context.WithCancelCause(ctx)
	go func() {
		select {
		case <-gone:
			cancel(errNoOne)
		case <-ctx.Done():
		}
	}()
	return ctx, func() { cancel(nil) }, true
}

// orNoOne is an answer as asked under ctx, errNoOne when the last view
// went while it waited.
func orNoOne(ctx context.Context) func(agenttool.Answer, error) (agenttool.Answer, error) {
	return func(ans agenttool.Answer, err error) (agenttool.Answer, error) {
		if errors.Is(context.Cause(ctx), errNoOne) {
			return agenttool.Answer{}, errNoOne
		}
		return ans, err
	}
}
