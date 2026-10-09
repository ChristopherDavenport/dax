// Package tool holds dax-coding's tools: read, write, edit, glob, grep,
// ls and bash. They are written against the agenttool contract, so the
// same values run under agentturn, under any other Open Responses loop,
// or behind an MCP server. They act through a workspace.Workspace,
// never os or os/exec, so they run unchanged on this machine
// (workspace.Local), in a container or on a remote runtime, and the
// policy's checks of their calls read that workspace's files too.
//
// Any extension uses this package for its own tools: Files to turn a
// path the model gives into the workspace's and confine it (ReadFile,
// WriteFile, Update, Stat, ReadDir), DefaultEnv for the environment of
// a local workspace's processes, and BashSubjects, PathSubjects and
// Analyzer to read a command line or a path as the policy does. Each of
// dax's tools makes the facts claim (agenttool.Factual) from that same
// analysis: what a call would touch, and bash's stamped plan; the
// session takes the policy's subjects and the stamp from the claims.
package tool

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/openresponses"
)

// Builtins returns read, write, edit, glob, grep, ls and bash, in the
// order the system prompt lists them. The file tools are confined to
// f's workspace; bash runs at its root but is not confined.
func Builtins(f *Files, maxRead int64, bash ...BashOption) []agenttool.Tool {
	return []agenttool.Tool{Read(f, WithMaxRead(maxRead)), Write(f), Edit(f, WithMaxRead(maxRead)), Glob(f), Grep(f), LS(f), Bash(f, append([]BashOption{WithMaxFile(maxRead)}, bash...)...)}
}

// ReadOnly returns the tools that only look: read, glob, grep and ls.
func ReadOnly(f *Files, maxRead int64) []agenttool.Tool {
	return []agenttool.Tool{Read(f, WithMaxRead(maxRead)), Glob(f), Grep(f), LS(f)}
}

// Text returns the text a tool produced, for tests and renderers. Text
// parts are shown as their text and any other part as its JSON.
func Text(r agenttool.Result) string {
	if r.Output.Text != "" || len(r.Output.Parts) == 0 {
		return r.Output.Text
	}
	var b strings.Builder
	for _, p := range r.Output.Parts {
		if t, ok := p.(*openresponses.InputText); ok {
			b.WriteString(t.Text)
			continue
		}
		j, _ := json.Marshal(p)
		b.Write(j)
		b.WriteByte('\n')
	}
	return b.String()
}

// truncate caps s at max bytes and appends a note when it does.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("\n... [truncated, %d bytes omitted]", len(s)-max)
}
