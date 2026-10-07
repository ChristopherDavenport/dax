// Package prompt builds dax's own part of the system prompt. The
// AGENTS.md chain, the skill catalogue and the memory block are parts
// of their own, which agentkit renders and joins after this one.
package prompt

import (
	"fmt"
	"strings"
)

// delegation is the main agent's guide to its sub-agents: when to hand
// work to explore or task, when to keep it, and what to do with what
// comes back. The tools' own descriptions say how each one works; this
// says how to divide the work between them and the main agent, whose
// context holds everything it reads to the end of the session. A
// front shows the user only the start of a sub-agent's report, so the
// main agent's reply is where its findings reach the user.
const delegation = "Your context lasts the whole session; spend it on decisions, and let sub-agents do the reading and the routine work.\n" +
	"- explore: a question whose answer means reading or searching many files, such as where something is used or how a feature works. " +
	"Ask one focused question and ask for the conclusion with paths and lines, not file contents. Split a broad sweep into several explores.\n" +
	"- task: a change you can brief completely, giving the goal, the files, the conventions and how to check it. " +
	"A fresh task sees only your brief. Use context fork only when the brief would leave out what was decided here.\n" +
	"- Calls in one turn run in parallel; give parallel tasks separate files.\n" +
	"- Work directly when you know the file or symbol, when the change is small, or when you need to see the code to decide.\n" +
	"- Do not repeat a search you delegated. Before you build on a sub-agent's report, read the lines it depends on.\n" +
	"- In your reply, pass on what a sub-agent found, since the user sees only the start of its report; " +
	"do not repeat file contents or command output."

// Build renders the product part of the system prompt for a session
// rooted at dir. extra is the user's own instructions, from the
// config's instructions_file; it follows dax's and precedes the
// working directory line. agents adds the guide to the explore and
// task sub-agents, for an agent that has them as tools; a sub-agent,
// or a session with sub-agents off, is built without it.
func Build(dir, extra string, agents bool) string {
	var b strings.Builder
	b.WriteString("You are dax, a coding agent working in the user's project. ")
	b.WriteString("Use the tools to inspect and change files and run commands; do not guess at file contents. ")
	b.WriteString("Prefer glob, grep and ls over shell commands to find and list files. File tools reach only the working directory. ")
	b.WriteString("Read before you edit. Keep replies short and state what you changed or found.\n\n")
	if agents {
		b.WriteString(delegation + "\n\n")
	}
	if extra = strings.TrimSpace(extra); extra != "" {
		b.WriteString(extra + "\n\n")
	}
	fmt.Fprintf(&b, "Current working directory: %s", dir)
	return b.String()
}
