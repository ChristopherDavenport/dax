package agent

import (
	"strings"

	"github.com/ChristopherDavenport/openresponses"
)

// maxPinnedBytes bounds a prompt a fold keeps verbatim: a pasted log
// past it is folded like the rest.
const maxPinnedBytes = 8 << 10

// pinPrompts is the fold's pin: every message the person wrote (a
// prompt, a steer, a follow-up, a note with an answer) up to
// maxPinnedBytes of text, so the task's exact words (its paths, its
// format, its constraints) survive any summary of them. The compaction
// entry records each one pinned. A message dax composes as the user's
// is told apart here: a command the user ran (userCommandLead); nested
// instructions must be too, when they land.
func pinPrompts(it openresponses.Item) bool {
	m, ok := it.(*openresponses.Message)
	if !ok || m.Role != openresponses.RoleUser {
		return false
	}
	text := m.Text()
	// A command the user ran is output, not a request (RunUserCommand).
	return len(text) <= maxPinnedBytes && !strings.HasPrefix(text, userCommandLead)
}
