package toolrender

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/ChristopherDavenport/agentconsole/toolview"
	"github.com/ChristopherDavenport/agentconsole/view"
)

// ---- read

// readRenderer shows a read as its path and the lines asked for, and
// once it has ended the lines it read, all of them, on its line, but
// for a read of the whole file, which is its path alone. A
// short read of a part of a file, a piece a person can take in at a
// glance, shows what it read with its line numbers, so the transcript
// has what the agent is working with. A longer one says how much it
// read, and a read of a whole file says that, however short the file:
// the path already names what was read.
type readRenderer struct{ paths }

func (r readRenderer) Head(c view.Call) (toolview.Line, bool) {
	a := parseArgs(c.Args)
	if !fits(c.Schema, "path", "offset", "limit") || a.str("path") == "" {
		return nil, false
	}
	head := toolview.Line{toolview.S(toolview.Emphasis, r.show(a.str("path")))}
	// Once it has ended, the lines it read, whatever was asked for: one
	// a cap cut short names where it stopped. A read of the whole file
	// names none: the path is all of it, and the body says so.
	if first, last, ok := readRange(c); ok {
		if wholeRead(c) {
			return head, true
		}
		if first == last {
			return append(head, toolview.S(toolview.Dim, fmt.Sprintf(":%d", first))), true
		}
		return append(head, toolview.S(toolview.Dim, fmt.Sprintf(":%d-%d", first, last))), true
	}
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

// readRange is the first and the last line an ended read read, by the
// numbers it wrote, and whether it read any.
func readRange(c view.Call) (first, last int, ok bool) {
	if c.State != view.CallEnded {
		return 0, 0, false
	}
	for _, l := range lines(c.Output) {
		if !numbered.MatchString(l) {
			continue
		}
		num, _, _ := strings.Cut(l, "\t")
		n, err := strconv.Atoi(strings.TrimSpace(num))
		if err != nil {
			continue
		}
		if !ok {
			first, ok = n, true
		}
		last = n
	}
	return first, last, ok
}

func (r readRenderer) Body(c view.Call, expanded bool) ([]toolview.Line, bool) {
	if msg, failed := failure(c); failed {
		return errorLines(msg, expanded), true
	}
	if c.State != view.CallEnded || expanded {
		return nil, false
	}
	n, note := 0, ""
	var shown [][2]string // the number and the text of a short read's lines
	for _, l := range lines(c.Output) {
		switch {
		case numbered.MatchString(l):
			n++
			if len(shown) <= shortRead {
				num, text, _ := strings.Cut(l, "\t")
				shown = append(shown, [2]string{strings.TrimSpace(num), text})
			}
		case strings.HasPrefix(l, "... ("):
			// "... (312 more lines; use offset=81)" or the byte cap.
			note = " " + strings.TrimPrefix(l, "... ")
		case strings.HasPrefix(l, "("):
			// "(file has 12 lines; offset 40 is past the end)"
			return []toolview.Line{dim(clip(l))}, true
		}
	}
	if wholeRead(c) {
		// The head names no lines; the count says how much it is.
		return []toolview.Line{dim("· whole file, " + plural(n, "line", "lines"))}, true
	}
	if n == 0 || n > shortRead {
		return []toolview.Line{dim("· " + plural(n, "line", "lines") + clip(note))}, true
	}
	// The numbers share a column, the width of the last one.
	width := len(shown[len(shown)-1][0])
	out := make([]toolview.Line, 0, len(shown)+1)
	for _, s := range shown {
		// A tab is drawn as four columns, so the indentation reads.
		text := strings.ReplaceAll(s[1], "\t", "    ")
		out = append(out, toolview.Line{
			toolview.S(toolview.Dim, fmt.Sprintf("%*s  ", width, s[0])),
			toolview.Code(toolview.Dim, syntaxOf(parseArgs(c.Args).str("path")), clip(text)),
		})
	}
	if note != "" {
		out = append(out, dim("…"+clip(note)))
	}
	return out, true
}

// Loud keeps a read on the screen: a short one is the one whose lines
// are drawn, and the client would fold it away with the calls that say
// nothing.
func (readRenderer) Loud() bool { return true }

// wholeRead reports whether an ended read read its whole file: from the
// top, with lines and with no note of more behind them.
func wholeRead(c view.Call) bool {
	if c.State != view.CallEnded || parseArgs(c.Args).num("offset") > 1 {
		return false
	}
	any := false
	for _, l := range lines(c.Output) {
		if strings.HasPrefix(l, "... (") {
			return false
		}
		any = any || numbered.MatchString(l)
	}
	return any
}

// ---- write

// writeRenderer shows a write as its path and the size of the content,
// and the content as added lines: its start collapsed, as an edit's
// diff is cut, and all of it behind ctrl+o.
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

// Loud keeps a write on the screen, and so what it wrote: its output, "wrote 120 bytes to
// x.go", is one line, and the client would fold it away with the calls
// that say nothing.
func (writeRenderer) Loud() bool { return true }

func (r writeRenderer) Body(c view.Call, expanded bool) ([]toolview.Line, bool) {
	var out []toolview.Line
	if msg, failed := failure(c); failed {
		out = errorLines(msg, expanded)
	}
	a := parseArgs(c.Args)
	if !a.complete {
		return out, out != nil
	}
	// "wrote 120 bytes to x.go" says nothing the head does not; what was
	// written is the content.
	content := lines(a.str("content"))
	for i, l := range content {
		if !expanded {
			if i == collapsedLines {
				out = append(out, more(len(content)-i))
				break
			}
			l = clip(l)
		}
		out = append(out, toolview.Line{toolview.S(toolview.Added, "+ "), toolview.Code(toolview.Added, syntaxOf(a.str("path")), l)})
	}
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

// Loud keeps an edit on the screen, and so its diff: its output, "edited
// x.go", is one line, and the client would fold it away with the calls
// that say nothing.
func (editRenderer) Loud() bool { return true }

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
	return append(out, diffLines(diff(lines(a.str("old_string")), lines(a.str("new_string"))), expanded, syntaxOf(a.str("path")))...), true
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

// Bare makes the head `$ <command>` stand in for the tool's name on the
// collapsed line, so a call reads `○ $ ls` and not `○ bash $ ls`.
func (bashRenderer) Bare() bool { return true }

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

// ---- dax-a2a's peers

// peerRenderer shows a call to a remote agent as the first line of the
// request, and the answer's first lines once it ends. A call that ended
// with the agent asking for input shows the question, which the model
// reads, rather than as a failure.
type peerRenderer struct{}

// needsInput is how tools/a2a's InputRequiredError begins.
const needsInput = "the agent needs more input"

func (peerRenderer) Head(c view.Call) (toolview.Line, bool) {
	return agentRenderer{props: []string{"input"}}.Head(c)
}

func (peerRenderer) Body(c view.Call, expanded bool) ([]toolview.Line, bool) {
	if msg, failed := failure(c); failed {
		if q, ok := strings.CutPrefix(msg, needsInput); ok {
			q = strings.TrimSpace(strings.TrimPrefix(q, ":"))
			if q == "" || strings.HasPrefix(q, "before it can continue") {
				return []toolview.Line{dim("asks for more input")}, true
			}
			out := []toolview.Line{{toolview.S(toolview.Dim, "asks: "), toolview.S(toolview.Emphasis, clip(lines(q)[0]))}}
			if rest := lines(q)[1:]; len(rest) > 0 {
				if !expanded {
					return append(out, more(len(rest))), true
				}
				for _, l := range rest {
					out = append(out, toolview.Line{toolview.S(toolview.Plain, l)})
				}
			}
			return out, true
		}
	}
	return agentRenderer{props: []string{"input"}}.Body(c, expanded)
}

// backgroundRenderer draws the background tool's call: the action, and
// for a start its command line as bash's head draws one; the result as
// the client draws any.
type backgroundRenderer struct{}

// Bare: every head names the tool itself ("background $ ls",
// "background stop 3"), so the client's name before it would say it twice.
func (backgroundRenderer) Bare() bool { return true }

func (backgroundRenderer) Head(c view.Call) (toolview.Line, bool) {
	a := parseArgs(c.Args)
	action := a.str("action")
	if !fits(c.Schema, "action", "command", "id") || action == "" {
		return nil, false
	}
	switch action {
	case "start":
		ls := lines(strings.TrimSpace(a.str("command")))
		return toolview.Line{toolview.S(toolview.Dim, "background $ "), toolview.S(toolview.Emphasis, ls[0])}, true
	case "output", "stop":
		return toolview.Line{toolview.S(toolview.Dim, "background "), toolview.S(toolview.Emphasis, fmt.Sprintf("%s %d", action, a.num("id")))}, true
	}
	return toolview.Line{toolview.S(toolview.Dim, "background "), toolview.S(toolview.Emphasis, action)}, true
}

func (backgroundRenderer) Body(view.Call, bool) ([]toolview.Line, bool) { return nil, false }
