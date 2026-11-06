package agent

import (
	"sync"

	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// maxPinnedBytes bounds a task a fold keeps verbatim: a pasted log past
// it is folded like the rest. It is agentturn compact's
// DefaultPersonBound, the bound of the person's own messages.
const maxPinnedBytes = 8 << 10

// tasks are the messages a fold keeps verbatim beside the person's own:
// the task an autonomous run was given (Drive's items, -p's prompt),
// which claims no person, since a script may have given it, but whose
// exact words (its paths, its format, its constraints) must survive any
// summary of them. The person's own messages, sent under a trigger of
// kind human, are agentturn's to keep (compact's default, #298); dax
// guesses at no other user message. Held by identity, as agentturn
// holds the person's: a copy is not the message.
type tasks struct {
	mu sync.Mutex
	m  map[*openresponses.Message]bool
}

// add marks the user messages of items within the bound.
func (t *tasks) add(items []openresponses.Item) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, it := range items {
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
