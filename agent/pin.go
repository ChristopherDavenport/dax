package agent

import (
	"github.com/ChristopherDavenport/openresponses"
)

// maxPinnedBytes bounds a prompt a fold keeps verbatim: a pasted log
// past it is folded like the rest.
const maxPinnedBytes = 8 << 10

// pinPrompts is the fold's pin: every message the person wrote (a
// prompt, a steer, a follow-up, a note with an answer) up to
// maxPinnedBytes of text, so the task's exact words (its paths, its
// format, its constraints) survive any summary of them. The compaction
// entry records each one pinned. Nothing dax composes is a user
// message today; one that is (a command's output the user ran, nested
// instructions) must be told apart here before it lands.
func pinPrompts(it openresponses.Item) bool {
	m, ok := it.(*openresponses.Message)
	if !ok || m.Role != openresponses.RoleUser {
		return false
	}
	return len(m.Text()) <= maxPinnedBytes
}
