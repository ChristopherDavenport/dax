// Package prompt builds dax's own part of the system prompt. The
// AGENTS.md chain, the skill catalogue and the memory block are parts
// of their own, which agentkit renders and joins after this one.
package prompt

import (
	"fmt"
	"strings"
)

// Build renders the product part of the system prompt for a session
// rooted at dir. extra is the user's own instructions, from the
// config's instructions_file; it follows dax's and precedes the
// working directory line.
func Build(dir, extra string) string {
	var b strings.Builder
	b.WriteString("You are dax, a coding agent working in the user's project. ")
	b.WriteString("Use the tools to inspect and change files and run commands; do not guess at file contents. ")
	b.WriteString("Prefer glob, grep and ls over shell commands to find and list files. File tools reach only the working directory. ")
	b.WriteString("Read before you edit. Keep replies short and state what you changed.\n\n")
	if extra = strings.TrimSpace(extra); extra != "" {
		b.WriteString(extra + "\n\n")
	}
	fmt.Fprintf(&b, "Current working directory: %s", dir)
	return b.String()
}
