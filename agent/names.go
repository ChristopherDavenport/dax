package agent

// Session names and the project's sessions from inside one (#164):
// /name writes agentsession's info entry, and /resume lists this
// project's sessions with their names and moves to one (Reopen).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ChristopherDavenport/agentsession"
)

// maxName bounds a session's name, which a listing shows on one line.
const maxName = 200

// SetName names the session: an info entry (agentsession's InfoEntry)
// appended to its record, which -list, /resume and the start line of a
// resume show; a later one replaces it. A name is one line of at most
// 200 bytes with no control characters. It waits for the run to end.
func (s *Session) SetName(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return errors.New("a session's name is not empty")
	case len(name) > maxName:
		return fmt.Errorf("a session's name is at most %d bytes", maxName)
	case !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl):
		return errors.New("a session's name is one line of text, without control characters")
	}
	if s.ag.State().Running {
		return errors.New("waits for the run to end")
	}
	_, err := s.store.Append(ctx, s.ID(), &agentsession.InfoEntry{Name: name})
	return err
}

// Name is the session's name, from the last info entry that set one;
// empty for none.
func (s *Session) Name() string {
	if s.Kit == nil || s.Kit.Session() == nil {
		return ""
	}
	return s.Kit.Session().Name()
}

// Sessions lists this project's sessions in the session's store, newest
// first, with their names: the top-level sessions recorded in the
// workspace the session's tools act in (the identity -list goes by),
// this one among them, and not those a continued_in link retired, nor
// an A2A caller's conversation (OpenPeer's, marked dax.origin "a2a"):
// that text is a caller's, not the person's to continue under their
// own mode. A caller's conversation recorded before the mark is listed.
func (s *Session) Sessions(ctx context.Context) ([]agentsession.Summary, error) {
	if !s.recorded {
		return nil, errors.New("no session store: nothing is kept to resume")
	}
	here := s.identity()
	f := agentsession.ListFilter{CWD: s.ws.Root(), TopLevel: true, WithNames: true, Current: true}
	var out []agentsession.Summary
	for sum, err := range s.store.List(ctx, f) {
		if err != nil {
			return out, err
		}
		if sessionOrigin(sum.Header) == originPeer {
			continue
		}
		if sameProject(workspaceOf(sum.Header), here) {
			out = append(out, sum)
		}
	}
	return out, nil
}

// sessionOrigin is the origin a header marks (headerOrigin); "" for the
// person's own session.
func sessionOrigin(h agentsession.Header) string {
	var o string
	if raw, ok := h.Extra[headerOrigin]; ok {
		_ = json.Unmarshal(raw, &o)
	}
	return o
}
