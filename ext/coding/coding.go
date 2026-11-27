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
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ChristopherDavenport/agentkit"

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
	"Read before you edit. " +
	"Start a server or a watcher with the background tool, not with & in bash, read its output with it, and stop it when done. " +
	"Keep shell searches to the working directory and paths you name; never search from / or $HOME. " +
	"After a change, run the checks the project's instructions name (AGENTS.md, a Makefile target) before calling it done, and say which ran and how they ended."

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
			var proc []tool.ProcessOption
			if e.Deliver != nil {
				proc = append(proc, tool.WithExitNotice(exitDelivery(e.Deliver)))
			}
			return tool.BuiltinsWith(e.Files, maxRead, proc)
		},
		ReadOnly: []string{"read", "glob", "grep", "ls", "bash"},
		// read_output reads the session's own kept outputs, so it runs
		// where the session does, through the kit.
		Owns: []string{"read_output"},
		Kit: func(env extension.Env) ([]agentkit.Option, error) {
			return []agentkit.Option{agentkit.WithTools(tool.ReadOutput(env.ToolOutput))}, nil
		},
		Matchers: extension.FixedMatchers(matchers()),
		Aliases:  aliases,
		Policy:   policy.Rules{Allow: strings.Fields(allowRules), Ask: strings.Fields(askRules())},
		ModeRules: map[string]policy.Rules{
			policy.ModeAcceptEdits: acceptEditsRules(),
			// Plan mode is for reading: no write or edit, over any allow.
			policy.ModePlan: {Deny: []string{"write", "edit"}},
		},
		// After a bash call that runs git, the checkouts it ran in are
		// read again, so the record says which commit, branch and
		// upstream the work moved to (agentsession's env checkouts).
		AfterToolCall: gitAfterBash,
		GrantRule:     grantRule,
		Lifts:         lifts,
		Instructions:  instructions,
		Renderers:     toolrender.Renderers,
		Summarizers:   toolrender.Summarizers,
	}
}

// ExitTrigger is the kind of the trigger a background process's exit is
// recorded under: the process's id as the ref, its command beside.
const ExitTrigger = "background-exit"

// exitDelivery tells the session of a background process that ended on
// its own: its command, how it ended, how long it ran, and the tail of
// its output, which the session frames as untrusted data. The session
// starts a run for it, or lets the run in flight take it.
func exitDelivery(deliver func(extension.Delivery) error) func(tool.ExitNotice) {
	return func(n tool.ExitNotice) {
		_ = deliver(extension.Delivery{
			Trigger: agentturn.Trigger{Kind: ExitTrigger, Source: "background", Ref: strconv.Itoa(n.ID),
				Extra: map[string]any{"command": clip(n.Command, 200)}},
			Header:   exitHeader(n),
			Payload:  exitPayload(n),
			MaxBytes: tool.NoticeTail,
		})
	}
}

// exitHeader is the first line of a process's exit, which names the
// process and says how it ended, in dax's words and not the process's.
func exitHeader(n tool.ExitNotice) string {
	how := fmt.Sprintf("exited with status %d", n.Status)
	switch {
	case n.Err != nil:
		how = fmt.Sprintf("ended abnormally (status %d: %v)", n.Status, n.Err)
	case n.Status == 0:
		how = "exited with status 0"
	}
	return fmt.Sprintf("[Background process %d %s after %s. Command: %s]", n.ID, how, n.Duration.Round(time.Second), strings.Join(strings.Fields(n.Command), " "))
}

// exitPayload is the tail of the output the model has not read.
func exitPayload(n tool.ExitNotice) string {
	n.Tail = strings.TrimRight(n.Tail, "\n")
	switch {
	case n.Tail == "" && n.Cut == 0:
		return "[no unread output]"
	case n.Cut > 0:
		return fmt.Sprintf("[%d earlier bytes of unread output left out; the last %d follow]\n%s", n.Cut, len(n.Tail), n.Tail)
	}
	return n.Tail
}

// clip is s cut to at most n bytes at a character.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}

// gitAfterBash is dax-coding's after-call hook: a bash call whose
// command runs a git command that can move a checkout (tool.GitMoves)
// has the session read the checkouts it ran in, or made, again
// (Env.CheckoutsMoved); one whose command the parser cannot read but
// that names git has every checkout read. It never changes the result.
func gitAfterBash(env extension.Env) (func(context.Context, agentturn.ToolResultInfo) (*agentturn.ToolOverride, error), error) {
	files := env.ToolEnv().Files
	if files == nil {
		return nil, nil
	}
	return func(ctx context.Context, info agentturn.ToolResultInfo) (*agentturn.ToolOverride, error) {
		if info.Call == nil || info.Call.Name != "bash" {
			return nil, nil
		}
		var args tool.BashArgs
		if err := json.Unmarshal(info.Args, &args); err != nil || args.Command == "" {
			return nil, nil
		}
		dirs, all := tool.GitMoves(args.Command, files)
		switch {
		case all:
			env.CheckoutsMoved(ctx)
		case len(dirs) > 0:
			env.CheckoutsMoved(ctx, dirs...)
		}
		return nil, nil
	}, nil
}
