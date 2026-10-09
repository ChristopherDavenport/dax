// Package coding is dax-coding, the extension that makes dax a coding
// agent: the read, write, edit, glob, grep, ls and bash tools, the rules
// dax ships for them, the matchers and aliases those rules are written
// with, the stamp that holds an auto-allowed bash call to the plan the
// policy approved, the prompt's guidance on using them, and their
// renderers in the terminal client. It is built only from what package
// extension offers any extension, and acts through the session's
// workspace.Workspace, so it works the same on this machine, in a
// container or on a remote runtime.
package coding

import (
	"strings"

	"github.com/ChristopherDavenport/agenttool"

	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/policy"
	"github.com/ChristopherDavenport/dax/tool"
	"github.com/ChristopherDavenport/dax/toolrender"
)

// Name is the extension's name, and so its rules' source:
// extension:dax-coding.
const Name = "dax-coding"

// instructions are dax-coding's part of the system prompt.
const instructions = "Use the tools to inspect and change files and run commands; do not guess at file contents. " +
	"Prefer glob, grep and ls over shell commands to find and list files. File tools reach only the working directory. " +
	"Read before you edit."

// New is dax-coding. maxRead is the most bytes of a
// file read scans, edit rewrites and the bash analyzer reads of a file a
// command names; zero is tool.DefaultMaxRead.
//
// Its tools are built over the session's ToolEnv, on its tool.Files,
// whose write lock every writing tool of the session shares. Each makes
// the facts claim (agenttool.Factual): what a call would touch, read in the
// workspace the tools act in, and for bash the stamped plan. The session
// takes the policy's subjects and the stamp from those claims; dax-coding
// ships only how its rules match (matchers), so the policy reads no
// machine but through the tools.
//
// read, glob, grep, ls and bash are ReadOnly, which the explore
// sub-agent gets too. bash is there though a command may write, because
// the policy holds it to looking: only a command the analyzer finds
// read-only runs unasked, and the rest ask, in explore as everywhere.
func New(maxRead int64) extension.Extension {
	return extension.Extension{
		Name: Name,
		Tools: func(e extension.ToolEnv) []agenttool.Tool {
			return tool.Builtins(e.Files, maxRead)
		},
		ReadOnly:     []string{"read", "glob", "grep", "ls", "bash"},
		Matchers:     extension.FixedMatchers(matchers()),
		Aliases:      aliases,
		Policy:       policy.Rules{Allow: strings.Fields(allowRules), Ask: strings.Fields(askRules())},
		Lifts:        lifts,
		Instructions: instructions,
		Renderers:    toolrender.Renderers,
	}
}
