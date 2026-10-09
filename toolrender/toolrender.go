// Package toolrender draws dax's tool calls in the terminal client: a
// [toolview.Renderer] for each of dax-coding's read, write, edit, glob,
// grep, ls and bash (Renderers), and for dax-agents' task and explore
// (SubAgents). Each extension hands its own through
// extension.Extension.Renderers, and a front gives the client the
// extensions' together (extension.Renderers) through
// console.WithToolRenderers.
//
// A renderer reads the record's facts about a call, never the tool, so
// it parses the arguments with its own structs and reads the output the
// tool wrote: numbered lines from read, path:line:text from grep, a last
// "[exit N]" line from bash, and "Error: …" from any tool that failed.
// It does not import package tool, since a renderer reads the record
// and not the tool; the tests run the real tools to keep the two in
// step.
//
// Collapsed, a call is a line and a short body: an edit's diff, the end
// of a failing command, a count for the rest. Expanded (ctrl+o), an
// edit's whole diff, a write's content and a command's whole output, and
// the client's raw output for the others.
package toolrender

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ChristopherDavenport/agentconsole/toolview"
	"github.com/ChristopherDavenport/agentconsole/view"
)

// Renderers are the renderers of dax-coding's tools, which show a path
// inside dir relative to it.
func Renderers(dir string) toolview.Renderers {
	p := paths{dir: dir}
	return toolview.Renderers{
		"read":  readRenderer{p},
		"write": writeRenderer{p},
		"edit":  editRenderer{p},
		"glob":  globRenderer{p},
		"grep":  grepRenderer{p},
		"ls":    lsRenderer{p},
		"bash":  bashRenderer{},
	}
}

// SubAgents are the renderers of dax-agents' task and explore calls: the
// brief, and the start of the report.
func SubAgents() toolview.Renderers {
	return toolview.Renderers{
		"task":    agentRenderer{props: []string{"input", "context", "model"}},
		"explore": agentRenderer{props: []string{"input"}},
	}
}

const (
	// collapsedLines is the most lines a collapsed body shows.
	collapsedLines = 8
	// tailLines is how much of a failing command's output a collapsed
	// call shows, from its end.
	tailLines = 3
	// reportLines is how much of a sub-agent's report a collapsed call
	// shows, from its start.
	reportLines = 2
	// lineRunes is the most of one line a collapsed body shows, so a
	// minified file or a long log line stays a row or two.
	lineRunes = 160
	// headRunes is the most of a sub-agent's brief the head shows.
	headRunes = 80
)

// args are a call's arguments as far as they have streamed: the
// top-level fields whose values are whole, and whether the object is.
type args struct {
	fields   map[string]any
	complete bool
}

// parseArgs reads raw, a JSON object that may still be streaming, up to
// the first value that is not whole. A head can name a write's path
// while its content streams.
func parseArgs(raw string) args {
	a := args{fields: map[string]any{}}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return a
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return a
		}
		key, ok := t.(string)
		if !ok {
			return a
		}
		var v any
		if err := dec.Decode(&v); err != nil {
			return a
		}
		a.fields[key] = v
	}
	if t, err := dec.Token(); err == nil && t == json.Delim('}') {
		a.complete = true
	}
	return a
}

// str is the string field key, "" when it is missing or not a string.
func (a args) str(key string) string {
	s, _ := a.fields[key].(string)
	return s
}

// num is the integer field key, 0 when it is missing or not a number.
func (a args) num(key string) int {
	n, ok := a.fields[key].(json.Number)
	if !ok {
		return 0
	}
	i, err := n.Int64()
	if err != nil {
		return 0
	}
	return int(i)
}

// flag is the boolean field key.
func (a args) flag(key string) bool {
	b, _ := a.fields[key].(bool)
	return b
}

// fits is whether schema is one a renderer reading props can read: it
// is nil, as a child call's is, or its properties name each of props. A
// tool of the same name from elsewhere, or an older shape, is declined.
func fits(schema json.RawMessage, props ...string) bool {
	if len(bytes.TrimSpace(schema)) == 0 {
		return true
	}
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(schema, &s) != nil {
		return false
	}
	for _, p := range props {
		if _, ok := s.Properties[p]; !ok {
			return false
		}
	}
	return true
}

// paths shows the paths of the file tools.
type paths struct{ dir string }

// show is p relative to the workspace when it is an absolute path inside
// it, as the model may give one, and "." for the root.
func (ps paths) show(p string) string {
	if p == "" {
		return "."
	}
	if ps.dir != "" && filepath.IsAbs(p) {
		if r, err := filepath.Rel(ps.dir, p); err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			return r
		}
	}
	return p
}

// failure is the message of a call that ended in a tool error, the
// "Error: …" text agenttool writes for one.
func failure(c view.Call) (string, bool) {
	if c.State != view.CallEnded {
		return "", false
	}
	return strings.CutPrefix(c.Output, "Error: ")
}

// errorLines is a failure's message as error lines: its first line
// collapsed, all of it expanded.
func errorLines(msg string, expanded bool) []toolview.Line {
	lines := toolview.Text(toolview.Error, msg)
	if !expanded && len(lines) > 1 {
		lines = lines[:1]
	}
	if !expanded {
		for i := range lines {
			lines[i] = toolview.Line{toolview.S(toolview.Error, clip(lines[i].String()))}
		}
	}
	return lines
}

// lines is s split into lines, a trailing newline dropped.
func lines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// clip cuts s to lineRunes runes.
func clip(s string) string { return clipTo(s, lineRunes) }

// clipTo cuts s to n runes, ending in … when it cut.
func clipTo(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// more is the dim line that says n more lines are behind ctrl+o.
func more(n int) toolview.Line {
	if n == 1 {
		return toolview.Line{toolview.S(toolview.Dim, "… 1 more line (ctrl+o)")}
	}
	return toolview.Line{toolview.S(toolview.Dim, fmt.Sprintf("… %d more lines (ctrl+o)", n))}
}

// dim is a line of one dim span.
func dim(s string) toolview.Line { return toolview.Line{toolview.S(toolview.Dim, s)} }

// plural is n and noun, the noun in the plural unless n is 1.
func plural(n int, noun, nouns string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %s", n, nouns)
}
