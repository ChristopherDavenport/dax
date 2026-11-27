package toolrender

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/ChristopherDavenport/agentconsole/toolview"
	"github.com/ChristopherDavenport/agentconsole/view"
)

// Summarizers are the summarizers of dax-coding's tools, for the line a
// collapsed run of quiet calls shows: bash's commands by their first
// words (`ran 3 commands: mkdir, cd, touch`), in place of the client's
// counts. It reads the calls alone and declines a run it cannot say
// anything useful of. A read has none: its renderer is loud, so a read
// is never in a run. dir is the workspace, for a summarizer that shows
// paths; none does now.
func Summarizers(dir string) toolview.Summarizers {
	_ = dir
	return toolview.Summarizers{
		"bash": bashSummarizer{},
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

// A bash output's last line is the status dax's bash tool writes
// (package tool, which toolrender does not import): "[exit N]", the same
// with "; a process it left running ..." after N, or "[killed after D]"
// for a timeout. The tool does not flag such a call as failed, so the
// summary reads the line itself.
var (
	exitStatus   = regexp.MustCompile(`^\[exit (-?\d+)(;.*)?\]$`)
	killedStatus = regexp.MustCompile(`^\[killed after [^\]]*\]$`)
)

// unsuccessful reports whether out ends in a status line that is not a
// clean exit: a non-zero exit code or a timeout. Only the last line
// counts, and nothing of the output but that fact is kept.
func unsuccessful(out string) bool {
	out = strings.TrimRight(out, " \t\r\n")
	last := out[strings.LastIndex(out, "\n")+1:]
	if killedStatus.MatchString(last) {
		return true
	}
	m := exitStatus.FindStringSubmatch(last)
	if m == nil {
		return false
	}
	n, err := strconv.Atoi(m[1])
	return err != nil || n != 0
}

// bashSummarizer sums up a run of commands as the programs they start.
type bashSummarizer struct{}

func (bashSummarizer) Summarize(calls []view.Call) (toolview.Line, bool) {
	var words []string
	failed := 0
	for _, c := range calls {
		if !fits(c.Schema, "command") {
			return nil, false
		}
		if unsuccessful(c.Output) {
			failed++
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
	line := toolview.Line{
		toolview.S(toolview.Dim, fmt.Sprintf("ran %d %s: ", len(calls), noun)),
		toolview.S(toolview.Emphasis, listed(words)),
	}
	if failed > 0 {
		// "failed" holds for a non-zero exit and a timeout alike.
		line = append(line, toolview.S(toolview.Error, fmt.Sprintf(" · %d failed", failed)))
	}
	return line, true
}
