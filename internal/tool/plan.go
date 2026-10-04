package tool

import (
	"regexp"
	"strings"
)

// word is one word of a command in the safe subset: its text with the
// quotes taken off, and whether it holds an unquoted * or ? that the
// shell would expand.
type word struct {
	text string
	glob bool
}

// stage is one simple command and the redirects it ends with.
type stage struct {
	words     []word
	redirects []string
}

// globWord is what a word the shell expands may be made of.
var globWord = regexp.MustCompile(`^[A-Za-z0-9._/*?-]+$`)

// redirectForms are the only redirects in the safe subset: standard
// error to standard output, and either stream to /dev/null.
var redirectForms = []string{"2>&1", "2>/dev/null", ">/dev/null"}

// plan is a command line of the safe subset: pipelines joined by &&,
// each a list of stages joined by |.
type plan struct {
	pipelines [][]stage
}

// parsePlan reads a command line in the safe subset, or says it is
// not. The subset is words of letters, digits and _ . / : @ % + , = -
// (and ^ or ~ after a word's first byte), single-quoted strings and
// double-quoted strings with no $, backtick or backslash, separated by
// spaces and tabs; * and ? in an unquoted word, which only ls accepts;
// the operators && and |; and, ending a stage, the redirects in
// redirectForms. Nothing else: no ;, &, ||, <, >, #, $, backtick,
// backslash, parentheses, braces, [, !, newline, control or non-ASCII
// byte, and no = in a stage's first word.
func parsePlan(cmd string) (*plan, bool) {
	var (
		p      plan
		cur    []stage
		st     stage
		w      strings.Builder
		glob   bool
		quoted bool // the current word has a quoted part
		inWord bool
		bad    bool
	)
	endWord := func() {
		if inWord {
			// A word the shell will expand is emitted bare, so it may hold
			// nothing but plain name characters and the * and ? that
			// expand, and no quoted part: a quoted part would be emitted
			// raw, and quotes are how a word hides ;, $( and > from the
			// reader.
			if glob && (quoted || !globWord.MatchString(w.String())) {
				bad = true
			}
			st.words = append(st.words, word{w.String(), glob})
			w.Reset()
			glob, quoted, inWord = false, false, false
		}
	}
	endStage := func() bool {
		endWord()
		if bad || len(st.words) == 0 || strings.Contains(st.words[0].text, "=") || st.words[0].glob {
			return false
		}
		cur = append(cur, st)
		st = stage{}
		return true
	}
	delim := func(i int) bool { // is cmd[i] the end of a token
		return i >= len(cmd) || cmd[i] == ' ' || cmd[i] == '\t' || cmd[i] == '|' || (cmd[i] == '&' && i+1 < len(cmd) && cmd[i+1] == '&')
	}
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case c == ' ' || c == '\t':
			endWord()
		case c == '\'':
			j := strings.IndexByte(cmd[i+1:], '\'')
			if j < 0 {
				return nil, false
			}
			body := cmd[i+1 : i+1+j]
			if !plain(body) {
				return nil, false
			}
			w.WriteString(body)
			inWord, quoted = true, true
			i += j + 1
		case c == '"':
			j := strings.IndexByte(cmd[i+1:], '"')
			if j < 0 {
				return nil, false
			}
			body := cmd[i+1 : i+1+j]
			if !plain(body) || strings.ContainsAny(body, "$`\\") {
				return nil, false
			}
			w.WriteString(body)
			inWord, quoted = true, true
			i += j + 1
		case c == '&':
			if i+1 >= len(cmd) || cmd[i+1] != '&' || !endStage() {
				return nil, false
			}
			p.pipelines = append(p.pipelines, cur)
			cur = nil
			i++
		case c == '|':
			if i+1 < len(cmd) && cmd[i+1] == '|' || !endStage() {
				return nil, false
			}
		case c == '>':
			// A redirect is a whole token: nothing before it but the 2
			// of 2>, and a delimiter after it.
			prefix := ""
			switch {
			case inWord && w.String() == "2" && !glob:
				prefix = "2"
			case inWord:
				return nil, false
			}
			matched := ""
			for _, f := range redirectForms {
				if strings.HasPrefix(f, prefix+">") && strings.HasPrefix(cmd[i-len(prefix):], f) && delim(i-len(prefix)+len(f)) {
					matched = f
					break
				}
			}
			if matched == "" || len(st.words) == 0 {
				return nil, false
			}
			w.Reset()
			inWord = false
			st.redirects = append(st.redirects, matched)
			i += len(matched) - len(prefix) - 1
		case c == '*' || c == '?':
			w.WriteByte(c)
			inWord, glob = true, true
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			strings.IndexByte("_./:@%+,=-", c) >= 0,
			(c == '^' || c == '~') && inWord:
			w.WriteByte(c)
			inWord = true
		default:
			return nil, false
		}
	}
	if !endStage() || bad {
		return nil, false
	}
	p.pipelines = append(p.pipelines, cur)
	return &p, true
}

// texts are a stage's words as text.
func (s stage) texts() []string {
	out := make([]string, len(s.words))
	for i, w := range s.words {
		out[i] = w.text
	}
	return out
}

// shellQuote quotes a word so bash reads it back as the same text.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// emitted is the words a stage is run with: its own, with the flags
// that switch off a repository's programs added to git diff, log and
// show (--no-ext-diff --no-textconv), and, for ls, the words that
// expand moved after a -- so that a file the expansion finds called -n
// or --output=x is a name and not an option.
func (st stage) emitted() []word {
	out := make([]word, 0, len(st.words)+3)
	switch st.words[0].text {
	case "git":
		for i, w := range st.words {
			out = append(out, w)
			if i == 1 {
				switch w.text {
				case "diff", "log", "show":
					out = append(out, word{text: "--no-ext-diff"}, word{text: "--no-textconv"})
				}
			}
		}
	case "ls":
		hasGlob := false
		for _, w := range st.words {
			hasGlob = hasGlob || w.glob
		}
		if !hasGlob {
			return st.words
		}
		// Flags and plain names first, then --, then what came after the
		// user's own --, then the words that expand.
		var head, after, globs []word
		dd := false
		for _, w := range st.words {
			switch {
			case !dd && !w.glob && w.text == "--":
				dd = true
			case dd:
				after = append(after, w)
			case w.glob:
				globs = append(globs, w)
			default:
				head = append(head, w)
			}
		}
		out = append(out, head...)
		out = append(out, word{text: "--"})
		out = append(out, after...)
		out = append(out, globs...)
	default:
		return st.words
	}
	return out
}

// render writes the plan as the command bash is given. Every word is
// quoted but the ones that are globs, which hold nothing but name
// characters and the * and ? that expand.
func (p *plan) render() string {
	var pipes []string
	for _, pl := range p.pipelines {
		var stages []string
		for _, st := range pl {
			var parts []string
			for _, w := range st.emitted() {
				if w.glob {
					parts = append(parts, w.text)
				} else {
					parts = append(parts, shellQuote(w.text))
				}
			}
			parts = append(parts, st.redirects...)
			stages = append(stages, strings.Join(parts, " "))
		}
		pipes = append(pipes, strings.Join(stages, " | "))
	}
	return strings.Join(pipes, " && ")
}
