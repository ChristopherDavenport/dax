package agent

// This file is the session's side of A2A as a callee: a host that serves
// the session (dax serve -a2a) runs each A2A caller's conversation on
// an agent of its own under the session's configuration, and records
// it in a session of its own. One session is one conversation, so a
// caller's conversation is never written into the served one.

import (
	"context"
	"errors"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
)

// PeerConfig is the configuration an A2A caller's conversation runs
// under when the session's host serves A2A beside it (agentturn's
// front/a2a): the session's agent's, so the same model and reasoning,
// extensions, policy and workspace, read when it is called. A call the
// policy asks about, or a tool's question, goes to the fronts answering
// the session's questions as the session's own do, naming the caller's
// conversation (PeerRecord.Attach), and with no front answering it is
// not asked: the policy's question becomes a refusal. Its ToolRecorder
// is nil; PeerRecord.Attach routes a run's records to the
// conversation's session.
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
// undoes it once the run is over.
func (p *PeerRecord) Attach(ctx context.Context, a *agentturn.Agent, who string) (context.Context, func(), error) {
	cfg := a.Config()
	cfg.ToolRecorder = p.rec.RecordFunc()
	if err := a.SetConfig(cfg); err != nil {
		return nil, nil, err
	}
	detach := p.rec.Attach(a)
	id := p.rec.SessionID()
	ctx = agentkit.ContextWithRecorder(ctx, p.rec)
	ctx = session.ContextWithSessionID(ctx, id)
	ctx = agentmemory.WithSession(ctx, id)
	ctx = context.WithValue(ctx, peerKey{}, who+" (session "+id+")")
	return ctx, detach, nil
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
