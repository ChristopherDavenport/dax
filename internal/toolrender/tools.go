package toolrender

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/ChristopherDavenport/agentconsole/toolview"
	"github.com/ChristopherDavenport/agentconsole/view"
)

// ---- read

// readRenderer shows a read as its path and the lines asked for, and
// what it read as a count.
type readRenderer struct{ paths }

func (r readRenderer) Head(c view.Call) (toolview.Line, bool) {
	a := parseArgs(c.Args)
	if !fits(c.Schema, "path", "offset", "limit") || a.str("path") == "" {
		return nil, false
	}
	head := toolview.Line{toolview.S(toolview.Emphasis, r.show(a.str("path")))}
	from, n := a.num("offset"), a.num("limit")
	switch {
	case from > 0 && n > 0:
		head = append(head, toolview.S(toolview.Dim, fmt.Sprintf(":%d-%d", from, from+n-1)))
	case from > 0:
		head = append(head, toolview.S(toolview.Dim, fmt.Sprintf(":%d-", from)))
	case n > 0:
		head = append(head, toolview.S(toolview.Dim, fmt.Sprintf(":1-%d", n)))
	}
	return head, true
}

// numbered is a line read wrote: its number, a tab and the text.
var numbered = regexp.MustCompile(`^ *\d+\t`)

func (r readRenderer) Body(c view.Call, expanded bool) ([]toolview.Line, bool) {
	if msg, failed := failure(c); failed {
		return errorLines(msg, expanded), true
	}
	if c.State != view.CallEnded || expanded {
		return nil, false
	}
	n, note := 0, ""
	for _, l := range lines(c.Output) {
		switch {
		case numbered.MatchString(l):
			n++
		case strings.HasPrefix(l, "... ("):
			// "... (312 more lines; use offset=81)" or the byte cap.
			note = " " + strings.TrimPrefix(l, "... ")
		case strings.HasPrefix(l, "("):
			// "(file has 12 lines; offset 40 is past the end)"
			return []toolview.Line{dim(clip(l))}, true
		}
	}
	return []toolview.Line{dim("· " + plural(n, "line", "lines") + clip(note))}, true
}

// ---- write

// writeRenderer shows a write as its path and the size of the content;
// the content itself is behind ctrl+o.
type writeRenderer struct{ paths }

func (r writeRenderer) Head(c view.Call) (toolview.Line, bool) {
	a := parseArgs(c.Args)
	if !fits(c.Schema, "path", "content") || a.str("path") == "" {
		return nil, false
	}
	head := toolview.Line{toolview.S(toolview.Emphasis, r.show(a.str("path")))}
	if _, ok := a.fields["content"]; ok {
		head = append(head, toolview.S(toolview.Dim, " ("+plural(len(lines(a.str("content"))), "line", "lines")+")"))
	}
	return head, true
}

func (r writeRenderer) Body(c view.Call, expanded bool) ([]toolview.Line, bool) {
	var out []toolview.Line
	if msg, failed := failure(c); failed {
		out = errorLines(msg, expanded)
	}
	a := parseArgs(c.Args)
	if !a.complete {
		return out, out != nil
	}
	if expanded {
		for _, l := range lines(a.str("content")) {
			out = append(out, toolview.Line{toolview.S(toolview.Added, "+ "+l)})
		}
	}
	// "wrote 120 bytes to x.go" says nothing the head does not.
	return out, true
}

// ---- edit

// editRenderer shows an edit as its path and the lines it adds and
// removes, and the change as a diff, so an edit under a permission shows
// what it would do.
type editRenderer struct{ paths }

func (r editRenderer) Head(c view.Call) (toolview.Line, bool) {
	a := parseArgs(c.Args)
	if !fits(c.Schema, "path", "old_string", "new_string") || a.str("path") == "" {
		return nil, false
	}
	head := toolview.Line{toolview.S(toolview.Emphasis, r.show(a.str("path")))}
	if a.complete {
		added, removed := counts(diff(lines(a.str("old_string")), lines(a.str("new_string"))))
		head = append(head,
			toolview.S(toolview.Plain, " "),
			toolview.S(toolview.Added, fmt.Sprintf("+%d", added)),
			toolview.S(toolview.Plain, " "),
			toolview.S(toolview.Removed, fmt.Sprintf("−%d", removed)))
	}
	return head, true
}

func (r editRenderer) Body(c view.Call, expanded bool) ([]toolview.Line, bool) {
	var out []toolview.Line
	if msg, failed := failure(c); failed {
		out = errorLines(msg, expanded)
		if !expanded {
			return out, true
		}
	}
	a := parseArgs(c.Args)
	if !a.complete {
		return out, out != nil
	}
	return append(out, diffLines(diff(lines(a.str("old_string")), lines(a.str("new_string"))), expanded)...), true
}

// ---- glob, grep, ls

// globRenderer shows a glob as its pattern and where, and what it found
// as a count.
type globRenderer struct{ paths }

func (r globRenderer) Head(c view.Call) (toolview.Line, bool) {
	a := parseArgs(c.Args)
	if !fits(c.Schema, "pattern", "path") || a.str("pattern") == "" {
		return nil, false
	}
	head := toolview.Line{toolview.S(toolview.Emphasis, a.str("pattern"))}
	if p := a.str("path"); p != "" && p != "." {
		head = append(head, toolview.S(toolview.Dim, " in "), toolview.S(toolview.Plain, r.show(p)))
	}
	return head, true
}

func (r globRenderer) Body(c view.Call, expanded bool) ([]toolview.Line, bool) {
	return counted(c, expanded, "(no files match)", "no files match", func(ls []string) string {
		return plural(len(ls), "file", "files")
	})
}

// grepRenderer shows a grep as its pattern, where and how, and what it
// found as matches and files.
type grepRenderer struct{ paths }

func (r grepRenderer) Head(c view.Call) (toolview.Line, bool) {
	a := parseArgs(c.Args)
	if !fits(c.Schema, "pattern", "path", "include", "ignore_case") || a.str("pattern") == "" {
		return nil, false
	}
	head := toolview.Line{toolview.S(toolview.Emphasis, "/"+a.str("pattern")+"/")}
	if p := a.str("path"); p != "" && p != "." {
		head = append(head, toolview.S(toolview.Dim, " in "), toolview.S(toolview.Plain, r.show(p)))
	}
	var how []string
	if inc := a.str("include"); inc != "" {
		how = append(how, inc)
	}
	if a.flag("ignore_case") {
		how = append(how, "-i")
	}
	if len(how) > 0 {
		head = append(head, toolview.S(toolview.Dim, " ("+strings.Join(how, ", ")+")"))
	}
	return head, true
}

// match is a line grep wrote: a path, a line number and the text.
var match = regexp.MustCompile(`^(.+?):\d+:`)

func (r grepRenderer) Body(c view.Call, expanded bool) ([]toolview.Line, bool) {
	return counted(c, expanded, "(no matches)", "no matches", func(ls []string) string {
		files := map[string]bool{}
		n := 0
		for _, l := range ls {
			if m := match.FindStringSubmatch(l); m != nil {
				files[m[1]] = true
				n++
			}
		}
		return plural(n, "match", "matches") + " in " + plural(len(files), "file", "files")
	})
}

// lsRenderer shows an ls as its directory, and what it holds as a count.
type lsRenderer struct{ paths }

func (r lsRenderer) Head(c view.Call) (toolview.Line, bool) {
	if !fits(c.Schema, "path") {
		return nil, false
	}
	a := parseArgs(c.Args)
	if !a.complete {
		return nil, false
	}
	return toolview.Line{toolview.S(toolview.Emphasis, r.show(a.str("path")))}, true
}

func (r lsRenderer) Body(c view.Call, expanded bool) ([]toolview.Line, bool) {
	return counted(c, expanded, "(empty)", "empty", func(ls []string) string {
		return plural(len(ls), "entry", "entries")
	})
}

// stopped is the note glob and grep end on when they hit their limit.
var stopped = regexp.MustCompile(`^\.\.\. \(stopped at (\d+) `)

// counted is the body of a search: the failure, or, ended and collapsed,
// none (the tool's text for nothing found) as the word for it, or what
// count says of the output's lines, with the limit when the search
// stopped at it. Expanded, the client shows the output.
func counted(c view.Call, expanded bool, nothing, none string, count func([]string) string) ([]toolview.Line, bool) {
	if msg, failed := failure(c); failed {
		return errorLines(msg, expanded), true
	}
	if c.State != view.CallEnded || expanded {
		return nil, false
	}
	if strings.TrimSpace(c.Output) == nothing {
		return []toolview.Line{dim("· " + none)}, true
	}
	ls := lines(c.Output)
	note := ""
	if n := len(ls); n > 0 {
		if m := stopped.FindStringSubmatch(ls[n-1]); m != nil {
			ls, note = ls[:n-1], " (stopped at "+m[1]+")"
		}
		if n := len(ls); n > 0 && ls[n-1] == "" {
			ls = ls[:n-1]
		}
	}
	return []toolview.Line{dim("· " + count(ls) + note)}, true
}

// ---- bash

// bashRenderer shows a command as the command, and what it printed as a
// count when it succeeded and its last lines when it failed.
type bashRenderer struct{}

func (bashRenderer) Head(c view.Call) (toolview.Line, bool) {
	a := parseArgs(c.Args)
	cmd := a.str("command")
	if !fits(c.Schema, "command", "timeout_seconds") || strings.TrimSpace(cmd) == "" {
		return nil, false
	}
	ls := lines(strings.TrimSpace(cmd))
	head := toolview.Line{toolview.S(toolview.Dim, "$ "), toolview.S(toolview.Emphasis, ls[0])}
	if len(ls) > 1 {
		head = append(head, toolview.S(toolview.Dim, fmt.Sprintf(" (+%d lines)", len(ls)-1)))
	}
	if t := a.num("timeout_seconds"); t > 0 {
		head = append(head, toolview.S(toolview.Dim, fmt.Sprintf(" (timeout %ds)", t)))
	}
	return head, true
}

// status is the last line bash writes: "[exit N]" or "[killed after D]".
var status = regexp.MustCompile(`^\[(exit (-?\d+)|killed after .+)\]$`)

func (bashRenderer) Body(c view.Call, expanded bool) ([]toolview.Line, bool) {
	switch c.State {
	case view.CallRunning:
		// The client shows what the command is printing.
		return nil, false
	case view.CallEnded:
	default:
		// Before it runs, a command of several lines shows the rest of
		// them: under a permission, what would run is read in full.
		a := parseArgs(c.Args)
		ls := lines(strings.TrimSpace(a.str("command")))
		if !a.complete || len(ls) < 2 {
			return nil, true
		}
		var out []toolview.Line
		for i, l := range ls[1:] {
			if !expanded && i == collapsedLines {
				out = append(out, more(len(ls)-1-i))
				break
			}
			out = append(out, toolview.Line{toolview.S(toolview.Emphasis, "  "+l)})
		}
		return out, true
	}
	ls := lines(c.Output)
	var m []string
	if len(ls) > 0 {
		m = status.FindStringSubmatch(ls[len(ls)-1])
	}
	if m == nil {
		// No status line: the tool failed before the command ran, or
		// this is not the output bash writes. A command that printed
		// "Error: …" and exited has its status line, so it is read
		// here only after that.
		if msg, failed := failure(c); failed {
			return errorLines(msg, expanded), true
		}
		return nil, false
	}
	ls = ls[:len(ls)-1]
	ok := m[2] == "0"
	if expanded {
		out := toolview.Text(toolview.Plain, strings.Join(ls, "\n"))
		role := toolview.Dim
		if !ok {
			role = toolview.Error
		}
		return append(out, toolview.Line{toolview.S(role, m[1])}), true
	}
	if ok {
		if len(ls) == 0 {
			return nil, true
		}
		return []toolview.Line{dim("· " + plural(len(ls), "line", "lines") + " (ctrl+o)")}, true
	}
	out := []toolview.Line{{toolview.S(toolview.Error, m[1])}}
	if n := len(ls) - tailLines; n > 0 {
		out = append(out, dim(fmt.Sprintf("… %d lines before", n)))
		ls = ls[n:]
	}
	for _, l := range ls {
		out = append(out, toolview.Line{toolview.S(toolview.Plain, clip(l))})
	}
	return out, true
}

// ---- task, explore

// agentRenderer shows a sub-agent's call as the first line of its brief,
// and its report's first lines once it ends. The client draws the calls
// the sub-agent makes under it.
type agentRenderer struct{ props []string }

func (r agentRenderer) Head(c view.Call) (toolview.Line, bool) {
	a := parseArgs(c.Args)
	in := strings.TrimSpace(a.str("input"))
	if !fits(c.Schema, r.props...) || in == "" {
		return nil, false
	}
	head := toolview.Line{toolview.S(toolview.Emphasis, clipTo(lines(in)[0], headRunes))}
	var how []string
	if ctx := a.str("context"); ctx == "fork" {
		how = append(how, "fork")
	}
	if m := a.str("model"); m == "main" {
		how = append(how, "main model")
	}
	if len(how) > 0 {
		head = append(head, toolview.S(toolview.Dim, " ("+strings.Join(how, ", ")+")"))
	}
	return head, true
}

func (r agentRenderer) Body(c view.Call, expanded bool) ([]toolview.Line, bool) {
	if msg, failed := failure(c); failed {
		return errorLines(msg, expanded), true
	}
	if c.State != view.CallEnded || expanded {
		return nil, false
	}
	var ls []string
	for _, l := range lines(c.Output) {
		if strings.TrimSpace(l) != "" {
			ls = append(ls, l)
		}
	}
	var out []toolview.Line
	for i, l := range ls {
		if i == reportLines {
			out = append(out, more(len(ls)-i))
			break
		}
		out = append(out, toolview.Line{toolview.S(toolview.Plain, clip(l))})
	}
	return out, true
}
