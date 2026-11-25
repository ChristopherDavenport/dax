package toolrender

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/ChristopherDavenport/agentconsole/toolview"
	"github.com/ChristopherDavenport/agentconsole/view"
)

// Summarizers are the summarizers of dax-coding's tools, for the line a
// collapsed run of quiet calls shows: bash's commands by their first
// words (`ran 3 commands: mkdir, cd, touch`) and read's files
// (`read a.go, b.go`), in place of the client's counts. Each reads the
// calls alone and declines a run it cannot say anything useful of. A
// path inside dir is shown relative to it.
func Summarizers(dir string) toolview.Summarizers {
	return toolview.Summarizers{
		"bash": bashSummarizer{},
		"read": readSummarizer{paths{dir: dir}},
	}
}

// summaryNames is the most names a summary lists before "+N more".
const summaryNames = 4

// wordRunes is the most of a command's first word a summary shows.
const wordRunes = 24

// A first word a summary may show: a program's name or path, with no
// quote, expansion, redirection or control character in it, since the
// line is drawn as it is and a command is the model's to write.
var plainWord = regexp.MustCompile(`^[A-Za-z0-9_./+:@~-]+$`)

// assignment is a leading `NAME=value` word, which is not the program
// and whose value may be a secret.
var assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// firstWord is the program a command starts, from its first line, past
// leading assignments; "" when the command is empty or starts with
// anything but a plain word (a quote, a substitution, a subshell).
func firstWord(cmd string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(cmd), "\n")
	for _, w := range strings.Fields(first) {
		if assignment.MatchString(w) {
			continue
		}
		if !plainWord.MatchString(w) {
			return ""
		}
		return clipTo(w, wordRunes)
	}
	return ""
}

// listed is "a, b, c, d +2 more" for names, at most summaryNames of them.
func listed(names []string) string {
	if len(names) <= summaryNames {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s +%d more", strings.Join(names[:summaryNames], ", "), len(names)-summaryNames)
}

// bashSummarizer sums up a run of commands as the programs they start.
type bashSummarizer struct{}

func (bashSummarizer) Summarize(calls []view.Call) (toolview.Line, bool) {
	var words []string
	for _, c := range calls {
		if !fits(c.Schema, "command") {
			return nil, false
		}
		if w := firstWord(parseArgs(c.Args).str("command")); w != "" {
			words = append(words, w)
		}
	}
	if len(words) == 0 {
		return nil, false
	}
	noun := "commands"
	if len(calls) == 1 {
		noun = "command"
	}
	return toolview.Line{
		toolview.S(toolview.Dim, fmt.Sprintf("ran %d %s: ", len(calls), noun)),
		toolview.S(toolview.Emphasis, listed(words)),
	}, true
}

// readSummarizer sums up a run of reads as the files, each once.
type readSummarizer struct{ paths }

func (r readSummarizer) Summarize(calls []view.Call) (toolview.Line, bool) {
	var names []string
	seen := map[string]bool{}
	for _, c := range calls {
		p := parseArgs(c.Args).str("path")
		if !fits(c.Schema, "path") || p == "" {
			return nil, false
		}
		p = clipTo(strings.Map(func(r rune) rune {
			if r < ' ' || r == 0x7f {
				return -1
			}
			return r
		}, r.show(p)), 60)
		if !seen[p] {
			seen[p] = true
			names = append(names, p)
		}
	}
	if len(names) == 0 {
		return nil, false
	}
	return toolview.Line{
		toolview.S(toolview.Dim, "read "),
		toolview.S(toolview.Emphasis, listed(names)),
	}, true
}
