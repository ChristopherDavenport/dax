package tool

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"

	"github.com/ChristopherDavenport/agentpolicy"
)

// sentinel prefixes a subject no rule names, so a call that carries it
// is decided by a bare rule, the fallback or the user, and never by an
// allow rule for a command. See BashSubjects.
const sentinel = "$(...) "

// BashSubjects returns the subjects splitter of the bash tool for a
// workspace rooted at dir. What it decides is who may auto-run:
//
//   - A command line inside the safe subset (see Analyzer) is one
//     subject per stage, each its words joined by single spaces, so an
//     allow rule for a command can match its stage. A stage of a command
//     the check governs (git, ls, cat ...) whose arguments are not
//     acceptable also gets a sentinel subject, which no rule matches,
//     so `git log --output=x` is not covered by a rule for `git log`.
//     So does a git stage in a repository whose configuration names a
//     program, and the question says which.
//   - Any other command line is cut into the parts a rule can still
//     name, so a deny or ask rule for rm reaches `git status; rm x`,
//     and a sentinel subject is added, so no allow rule for a command
//     ever covers it. The cut is for the question the user is asked and
//     for deny and ask rules; it is a best-effort reading of bash, not
//     a parser, and nothing is allowed because of it.
//
// A command with an unterminated quote is an error, which blocks the
// call.
func BashSubjects(dir string, maxFile int64) agentpolicy.Subjects {
	an := &Analyzer{Dir: dir, MaxFile: maxFile}
	return func(args json.RawMessage) ([]agentpolicy.Subject, error) {
		var in struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, err
		}
		mk := func(tool, field, match, text string) agentpolicy.Subject {
			a, _ := json.Marshal(map[string]string{field: match})
			return agentpolicy.Subject{Args: a, Tool: tool, Text: text}
		}
		cmd := strings.TrimSpace(in.Command)
		if cmd == "" {
			return nil, errors.New("command is empty")
		}
		if c := an.Check(context.Background(), cmd); c.Parsed {
			var out []agentpolicy.Subject
			for _, st := range c.Stages {
				out = append(out, mk("", "command", st.Match, st.Text))
				if st.Governed && !st.OK {
					out = append(out, mk("", "command", sentinel+st.Text, st.Text+"  ["+st.Why+"]"))
				}
			}
			return out, nil
		}
		parts, targets, _, err := splitShell(cmd)
		if err != nil {
			return nil, err
		}
		var out []agentpolicy.Subject
		for _, p := range parts {
			out = append(out, mk("", "command", p, p))
		}
		for _, t := range targets {
			out = append(out, mk("write", "path", t, t))
		}
		return append(out, mk("", "command", sentinel+cmd, cmd)), nil
	}
}

// splitShell cuts a command line the way a reader would, for the
// subjects of a command that is asked about. It follows quotes,
// $'...' strings, comments and the redirect forms, and reports
// whether anything it cannot follow (substitution) was there.
func splitShell(s string) (parts, redirects []string, opaque bool, err error) {
	var cur strings.Builder
	flush := func() {
		if t := strings.TrimSpace(cur.String()); t != "" {
			parts = append(parts, t)
		}
		cur.Reset()
	}
	wordStart := true // the next byte begins a word
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			cur.WriteByte(c)
			switch {
			case c == quote:
				quote = 0
			case quote == '"' && c == '\\' && i+1 < len(s):
				i++
				cur.WriteByte(s[i])
			case quote == '"' && (c == '`' || (c == '$' && i+1 < len(s) && s[i+1] == '(')):
				opaque = true
			}
			continue
		}
		start := wordStart
		wordStart = false
		switch c {
		case '\'', '"':
			quote = c
			cur.WriteByte(c)
		case '\\':
			cur.WriteByte(c)
			if i+1 >= len(s) {
				opaque = true
				break
			}
			i++
			cur.WriteByte(s[i])
		case '`':
			opaque = true
			cur.WriteByte(c)
		case '$':
			cur.WriteByte(c)
			if i+1 >= len(s) {
				break
			}
			switch s[i+1] {
			case '(':
				opaque = true
			case '\'':
				// $'...': backslash escapes, \' does not close it.
				opaque = true
				i++
				cur.WriteByte('\'')
				for i+1 < len(s) {
					i++
					cur.WriteByte(s[i])
					if s[i] == '\\' && i+1 < len(s) {
						i++
						cur.WriteByte(s[i])
						continue
					}
					if s[i] == '\'' {
						break
					}
					if i+1 >= len(s) {
						return nil, nil, false, errors.New("unterminated quote in command")
					}
				}
			case '"':
				// $"...": a double-quoted string.
				opaque = true
				i++
				quote = '"'
				cur.WriteByte('"')
			}
		case '#':
			if start {
				// A comment runs to the end of the line.
				for i+1 < len(s) && s[i+1] != '\n' {
					i++
				}
				break
			}
			cur.WriteByte(c)
		case ' ', '\t':
			cur.WriteByte(c)
			wordStart = true
		case ';', '\n', '|':
			flush()
			wordStart = true
		case '&':
			if i+1 < len(s) && s[i+1] == '>' { // &> file, &>> file
				i++
				if i+1 < len(s) && s[i+1] == '>' {
					i++
				}
				i, redirects = readTarget(s, i+1, redirects, &opaque)
				break
			}
			flush()
			wordStart = true
		case '<':
			if i+1 < len(s) && (s[i+1] == '(' || s[i+1] == '<') {
				opaque = true
			}
			cur.WriteByte(c)
		case '>':
			if i+1 < len(s) && s[i+1] == '(' {
				opaque = true
			}
			if i+1 < len(s) && (s[i+1] == '>' || s[i+1] == '|') {
				i++
			}
			if i+1 < len(s) && s[i+1] == '&' {
				// >&N and >&- duplicate or close a descriptor; >&word is
				// bash's spelling of > word 2>&1.
				j := i + 2
				for j < len(s) && s[j] >= '0' && s[j] <= '9' {
					j++
				}
				if j > i+2 && (j == len(s) || strings.IndexByte(" \t\n;&|<>()", s[j]) >= 0) || (j == i+2 && j < len(s) && s[j] == '-') {
					cur.WriteByte(c)
					cur.WriteString(s[i+1 : j])
					if j < len(s) && s[j] == '-' {
						cur.WriteByte('-')
						j++
					}
					i = j - 1
					break
				}
				i++
			}
			dropFD(&cur)
			i, redirects = readTarget(s, i+1, redirects, &opaque)
		default:
			cur.WriteByte(c)
		}
	}
	if quote != 0 {
		return nil, nil, false, errors.New("unterminated quote in command")
	}
	flush()
	return parts, redirects, opaque, nil
}

// dropFD removes a file descriptor number a redirect operator
// carried, the 2 of 2>file, from the end of the subcommand.
func dropFD(cur *strings.Builder) {
	t := cur.String()
	if n := len(t); n >= 1 && t[n-1] >= '0' && t[n-1] <= '9' && (n == 1 || t[n-2] == ' ') {
		cur.Reset()
		cur.WriteString(t[:n-1])
	}
}

// readTarget reads the word after a redirect operator at s[i:], records
// it unless it is /dev/null, and returns the index of its last byte.
func readTarget(s string, i int, redirects []string, opaque *bool) (int, []string) {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	start := i
	var quote byte
	for i < len(s) {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			i++
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			i++
			continue
		}
		if strings.IndexByte(" \t\n;&|<>()", c) >= 0 {
			break
		}
		i++
	}
	target := strings.Trim(s[start:i], `'"`)
	switch {
	case target == "":
		*opaque = true
	case target == "/dev/null":
	case strings.ContainsAny(target, "$`*?"):
		*opaque = true
		redirects = append(redirects, target)
	default:
		redirects = append(redirects, target)
	}
	return i - 1, redirects
}

// PathSubjects returns the subjects splitter of a file tool: its one
// subject is the path in field, normalised against the workspace dir
// (see NormalizePath), so a rule written for docs/** meets
// docs/../.git/x as .git/x, ./.env and a/../.env as .env and the
// absolute path of a file as the same name. Links are not followed:
// the rule matches the name the model used, and the tool's own
// confinement refuses a link that leaves. A path that leaves the
// workspace gets a subject no rule names, so it asks. When the field is
// absent the subject is def, which is the directory a search defaults to.
func PathSubjects(dir, field, def string) agentpolicy.Subjects {
	real, _ := filepath.EvalSymlinks(dir)
	return func(args json.RawMessage) ([]agentpolicy.Subject, error) {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(args, &m); err != nil {
			return nil, err
		}
		var raw string
		if v, ok := m[field]; ok {
			if err := json.Unmarshal(v, &raw); err != nil {
				return nil, err
			}
		}
		if raw == "" {
			raw = def
		}
		rel, ok := NormalizePath(dir, real, raw)
		if !ok {
			rel = sentinel + raw
		}
		a, _ := json.Marshal(map[string]string{"path": rel})
		return []agentpolicy.Subject{{Args: a, Text: raw}}, nil
	}
}
