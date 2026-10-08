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
	"context"
	"strings"

	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"

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
// Everything it builds is built over the session's ToolEnv: the tools
// over its tool.Files, whose write lock every writing tool of the
// session shares, and the matchers and the bash stamp over the same
// Files, so the policy looks at the workspace the tools act in and no
// other.
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
		ReadOnly: []string{"read", "glob", "grep", "ls", "bash"},
		Matchers: func(e extension.ToolEnv) map[string]agentpolicy.ToolMatcher {
			return matchers(e.Files, maxRead)
		},
		Aliases: aliases,
		Policy:  policy.Rules{Allow: strings.Fields(allowRules), Ask: strings.Fields(askRules())},
		Lifts:   lifts,
		BeforeToolCall: func(e extension.ToolEnv) func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
			return stampBash(&tool.Analyzer{Files: e.Files, MaxFile: maxRead})
		},
		Instructions: instructions,
		Renderers:    toolrender.Renderers,
	}
}

// stampBash is the hook that stamps an auto-allowed bash call with the
// plan the policy approved, and takes a stamp the model made off any
// other. It decides nothing itself: its Allow folds under the policy's
// verdict, so it never lets run a call the policy asks about.
func stampBash(an *tool.Analyzer) func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
	return func(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
		if info.Call == nil || info.Call.Name != "bash" {
			return nil, nil
		}
		args, changed, err := tool.StampArgs(ctx, an, info.Args)
		if err != nil || !changed {
			return nil, nil // not JSON: the tool will say so
		}
		return &agentturn.ToolDecision{Action: agentturn.Allow, Args: args}, nil
	}
}
