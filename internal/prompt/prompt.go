// Package prompt builds the session's part of the system prompt: the
// role line, the extensions' instructions and the user's, and the
// working directory. The AGENTS.md chain, the skill catalogue and the
// memory block are parts of their own, which agentkit renders and
// joins after this one.
package prompt

import (
	"fmt"
	"strings"
)

// Build renders the session's part of the system prompt for a session
// rooted at dir, for an agent called name (empty is dax): the role
// line, then extra, the extensions' instructions in order and the
// user's own from the config's instructions_file, then the working
// directory. An empty extra is left out.
func Build(name, dir string, extra ...string) string {
	if name == "" {
		name = "dax"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, a coding agent working in the user's project. ", name)
	b.WriteString("Keep replies short and state what you changed or found.\n\n")
	for _, e := range extra {
		if e = strings.TrimSpace(e); e != "" {
			b.WriteString(e + "\n\n")
		}
	}
	fmt.Fprintf(&b, "Current working directory: %s", dir)
	return b.String()
}
