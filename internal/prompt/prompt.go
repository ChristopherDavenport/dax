// Package prompt builds dex's own part of the system prompt. The
// AGENTS.md chain, the skill catalogue and the memory block are parts
// of their own, which agentkit renders and joins after this one.
package prompt

import (
	"fmt"
	"strings"
)

// Build renders the product part of the system prompt for a session
// rooted at dir.
func Build(dir string) string {
	var b strings.Builder
	b.WriteString("You are dex, a coding agent working in the user's project. ")
	b.WriteString("Use the tools to inspect and change files and run commands; do not guess at file contents. ")
	b.WriteString("Read before you edit. Keep replies short and state what you changed.\n\n")
	fmt.Fprintf(&b, "Current working directory: %s", dir)
	return b.String()
}
