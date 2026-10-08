// Package tool holds dax's built-in tools: read, write, edit, glob, grep, ls and bash.
// They are written against the agenttool contract, so the same values
// run under agentturn, under any other Open Responses loop, or behind
// an MCP server.
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
// ws; bash runs in its directory but is not confined.
func Builtins(ws *Workspace, maxRead int64, bash ...BashOption) []agenttool.Tool {
	return []agenttool.Tool{Read(ws, WithMaxRead(maxRead)), Write(ws), Edit(ws, WithMaxRead(maxRead)), Glob(ws), Grep(ws), LS(ws), Bash(ws.Dir(), append([]BashOption{WithMaxFile(maxRead)}, bash...)...)}
}

// ReadOnly returns the tools that only look: read, glob, grep and ls.
func ReadOnly(ws *Workspace, maxRead int64) []agenttool.Tool {
	return []agenttool.Tool{Read(ws, WithMaxRead(maxRead)), Glob(ws), Grep(ws), LS(ws)}
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
