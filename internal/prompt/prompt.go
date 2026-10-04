// Package prompt builds dex's own part of the system prompt. The
// AGENTS.md chain, the skill catalogue and the memory block are parts
// of their own, which agentkit renders and joins after this one.
package prompt

import (
	"fmt"
	"strings"
)

// Build renders the product part of the system prompt for a session
// rooted at dir. extra is the user's own instructions, from the
// config's instructions_file; it follows dex's and precedes the
// working directory line.
func Build(dir, extra string) string {
	var b strings.Builder
	b.WriteString("You are dex, a coding agent working in the user's project. ")
	b.WriteString("Use the tools to inspect and change files and run commands; do not guess at file contents. ")
	b.WriteString("Prefer glob, grep and ls over shell commands to find and list files. File tools reach only the working directory. ")
	b.WriteString("Never run a broad, unbounded shell search such as find /; keep any find scoped to the working directory and known paths. ")
	b.WriteString("Before edit, confirm the target string appears exactly once, with grep; keep old_string minimal and unique. ")
	b.WriteString("After changing Go files, run go build ./..., then go vet, then the relevant tests before treating the change as done. ")
	b.WriteString("Prefer explore for broad read-only investigation, and delegate self-contained coding tasks, so their output stays out of the main conversation. ")
	b.WriteString("Read before you edit. Keep replies short and state what you changed.\n\n")
	if extra = strings.TrimSpace(extra); extra != "" {
		b.WriteString(extra + "\n\n")
	}
	fmt.Fprintf(&b, "Current working directory: %s", dir)
	return b.String()
}
