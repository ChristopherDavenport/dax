package agent

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// maxPinnedBytes bounds a task a fold keeps verbatim: a pasted log past
// it is folded like the rest. It is agentturn compact's
// DefaultPersonBound, the bound of the person's own messages.
const maxPinnedBytes = 8 << 10

// tasks are the messages a fold keeps verbatim beside the person's own:
// the task an autonomous run was given (Drive's items, -p's prompt),
// which claims no person, since a script may have given it, and the
// request a person chose to send through a prompt template or an MCP
// prompt (Template, MCPPrompt), which is composed and so not their
// words, but whose exact words (its paths, its format, its
// constraints) must survive any summary of them (#173). The person's
// own messages, sent under a trigger of kind human, are agentturn's to
// keep (compact's default, #298). Held by identity, as agentturn holds
// the person's: a copy is not the message.
type tasks struct {
	mu sync.Mutex
	m  map[*openresponses.Message]bool
}

// add marks the user messages of items within the bound, a marked one
// (agentturn.WithProvenance) by the message it wraps, which the loop
// appends.
func (t *tasks) add(items []openresponses.Item) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, it := range items {
		it, _ = agentturn.Unhide(it)
		m, ok := it.(*openresponses.Message)
		if !ok || m.Role != openresponses.RoleUser || len(m.Text()) > maxPinnedBytes {
			continue
		}
		if t.m == nil {
			t.m = map[*openresponses.Message]bool{}
		}
		t.m[m] = true
	}
}

// pin is the fold's pin (compact.WithPin): a task added, which the
// compaction entry records as pinned.
func (t *tasks) pin(it openresponses.Item) bool {
	m, ok := it.(*openresponses.Message)
	if !ok {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.m[m]
}

// pinTasks marks items as c's task to keep through a fold, when c is a
// Session.
func pinTasks(c agentturn.Control, items []openresponses.Item) {
	if s, ok := c.(*Session); ok {
		s.tasks.add(items)
	}
}

// unclaimed returns the user messages of transcript that the record at
// sess's leaf holds as unclaimed input, for the agent to take as the
// person's own (Agent.SetPersonMessages), so a fold keeps them within
// agentturn's bound and total for those: a typed line a record written
// before format 0.21 claims for nobody, and -p's prompt after a resume,
// which tasks no longer holds. agentsession 0.21 lets a host treat an
// unclaimed message so (WordsUnclaimed). Not a message composed (from
// instruction layers, a peer link, or under a trigger of another
// kind), hidden or copied, and not a ! command's output, which an
// older dax sent unclaimed too. The transcript's messages are not the
// record's, so they are matched by their JSON; one that matches a
// message the record holds otherwise as well is left out.
func unclaimed(sess *agentsession.Session, transcript agentturn.Transcript) agentturn.PersonMessages {
	var out agentturn.PersonMessages
	for _, m := range recordedAs(sess, transcript, func(ie *agentsession.ItemEntry, m *openresponses.Message, w agentsession.Words) bool {
		return w == agentsession.WordsUnclaimed && !strings.HasPrefix(m.Text(), userCommandLead)
	}) {
		if out == nil {
			out = agentturn.PersonMessages{}
		}
		out[m] = true
	}
	return out
}

// requested returns the user messages of transcript that the record at
// sess's leaf holds as a prompt template's or an MCP prompt's request
// (Template, MCPPrompt): composed, from those layers alone, under the
// person's trigger or one of no kind (the recorder keeps a composed
// message's trigger without its kind). Sending one pinned it (tasks); after a
// resume or a head move the record says which they were, so they are
// pinned again.
func requested(sess *agentsession.Session, transcript agentturn.Transcript) []openresponses.Item {
	var out []openresponses.Item
	for _, m := range recordedAs(sess, transcript, func(ie *agentsession.ItemEntry, _ *openresponses.Message, w agentsession.Words) bool {
		layers := ie.Layers()
		if w != agentsession.WordsComposed || len(layers) == 0 || ie.FromLink != "" || ie.Source != nil && ie.Source.Kind != "" && ie.Source.Kind != agentsession.TriggerHuman {
			return false
		}
		for _, l := range layers {
			if l.Source != templateSource && l.Source != mcpPromptSource {
				return false
			}
		}
		return true
	}) {
		out = append(out, m)
	}
	return out
}

// recordedAs returns the user messages of transcript that the record at
// sess's leaf holds in an entry take accepts, visible and not copied.
// The transcript's messages are not the record's, so they are matched
// by their JSON; one that matches a message the record holds otherwise
// as well is left out, and so is one the record holds as the person's.
func recordedAs(sess *agentsession.Session, transcript agentturn.Transcript, take func(*agentsession.ItemEntry, *openresponses.Message, agentsession.Words) bool) []*openresponses.Message {
	if sess == nil || len(transcript) == 0 {
		return nil
	}
	taken, other := map[string]bool{}, map[string]bool{}
	for _, e := range sess.Path(sess.Leaf()) {
		ie, ok := e.(*agentsession.ItemEntry)
		if !ok {
			continue
		}
		m, ok := ie.Item.(*openresponses.Message)
		if !ok || m.Role != openresponses.RoleUser {
			continue
		}
		data, err := json.Marshal(m)
		if err != nil {
			continue
		}
		switch w := agentsession.WordsOf(ie); {
		case w == agentsession.WordsPerson:
		case ie.CopiedFrom == nil && (ie.Visible == nil || *ie.Visible) && take(ie, m, w):
			taken[string(data)] = true
		default:
			other[string(data)] = true
		}
	}
	var out []*openresponses.Message
	for _, it := range transcript {
		m, ok := it.(*openresponses.Message)
		if !ok || m.Role != openresponses.RoleUser {
			continue
		}
		data, err := json.Marshal(m)
		if err != nil || !taken[string(data)] || other[string(data)] {
			continue
		}
		out = append(out, m)
	}
	return out
}

// claimUnclaimed gives the agent the record's unclaimed messages of its
// transcript as the person's own (unclaimed), and pins its templates'
// and MCP prompts' requests again (requested), after a resume or a head
// move seeded it from the record.
func (s *Session) claimUnclaimed() {
	st := s.ag.State().Transcript
	if m := unclaimed(s.Kit.Session(), st); len(m) > 0 {
		// Seeded between runs: ErrRunning cannot happen.
		_ = s.ag.SetPersonMessages(m)
	}
	s.tasks.add(requested(s.Kit.Session(), st))
}
