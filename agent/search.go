package agent

import (
	"context"
	"errors"

	"github.com/ChristopherDavenport/agentsession"
)

// ErrPeerSearch is a search of the session store refused because an
// A2A caller's conversation asked for it: the store is the person's,
// every conversation they had in it, and a caller reads none of them.
var ErrPeerSearch = errors.New("an A2A caller cannot search this host's conversations")

// SearchScope confines a search of the session store to what the
// call's caller may read, as agentkit's SearchScope asks of a product
// that serves several principals over one store. The person reads the
// top-level sessions of this project: those recorded in the workspace
// the session's tools act in (the identity -list and memory go by),
// not the sub-agents' sessions, which their parents' calls already
// hold. An A2A caller's conversation, and its sub-agents', reads none:
// the call fails with ErrPeerSearch before anything is searched.
//
// It narrows q, the call's own, and never widens it.
func (s *Session) SearchScope(ctx context.Context, q *agentsession.Query) error {
	if _, peer := ctx.Value(peerKey{}).(string); peer {
		return ErrPeerSearch
	}
	here := s.identity()
	q.Filter.CWD = s.ws.Root()
	q.Filter.TopLevel = true
	if s.store == nil {
		return nil
	}
	// A path is one project's only with its kind and ref: two boxes at
	// /work are two projects. The headers say which, so the sessions
	// read are those whose recorded workspace is this one's.
	f := q.Filter
	f.Limit, f.WithNames, f.Current = 0, false, false
	ok := map[string]bool{}
	for sum, err := range s.store.List(ctx, f) {
		if err != nil {
			return err
		}
		if sameProject(workspaceOf(sum.Header), here) {
			ok[sum.Header.ID] = true
		}
	}
	prev := q.Readable
	q.Readable = func(id string) bool {
		return ok[id] && (prev == nil || prev(id))
	}
	return nil
}
