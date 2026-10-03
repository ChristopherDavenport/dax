package tool

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/ChristopherDavenport/agentpolicy"
)

// BashSubjects splits a bash call into the subjects a policy decides,
// so that `git status && rm -rf /` is two subcommands and a rule that
// allows the first does not allow the second. It is a splitter, not a
// shell parser, and it fails toward asking:
//
//   - the command is cut at unquoted ; & | && || and newlines;
//   - a redirect to a file adds a subject for the write tool on its
//     target, so `git log > ~/.bashrc` is decided by the write rules;
//   - command substitution, process substitution, backticks, a
//     here-document and a trailing backslash add a subject no
//     command rule matches, so the call is asked about whatever the
//     rest of it says;
//   - an unterminated quote is an error, which blocks the call.
func BashSubjects(args json.RawMessage) ([]agentpolicy.Subject, error) {
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, err
	}
	parts, targets, opaque, err := splitShell(in.Command)
	if err != nil {
		return nil, err
	}
	mk := func(tool, field, text string) agentpolicy.Subject {
		a, _ := json.Marshal(map[string]string{field: text})
		return agentpolicy.Subject{Args: a, Tool: tool, Text: text}
	}
	var out []agentpolicy.Subject
	for _, p := range parts {
		out = append(out, mk("", "command", p))
	}
	for _, t := range targets {
		out = append(out, mk("write", "path", t))
	}
	if opaque {
		// Nothing a rule names starts with this, so only a bare ask,
		// the default or a bare allow decides it.
		out = append(out, mk("", "command", "$(...) "+strings.TrimSpace(in.Command)))
	}
	if len(out) == 0 {
		return nil, errors.New("command is empty")
	}
	return out, nil
}

// splitShell cuts a command line. See BashSubjects.
func splitShell(s string) (parts, redirects []string, opaque bool, err error) {
	var cur strings.Builder
	flush := func() {
		if t := strings.TrimSpace(cur.String()); t != "" {
			parts = append(parts, t)
		}
		cur.Reset()
	}
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
			if i+1 < len(s) && s[i+1] == '(' {
				opaque = true
			}
			cur.WriteByte(c)
		case ';', '\n', '|':
			flush()
		case '&':
			if i+1 < len(s) && s[i+1] == '>' { // &> file
				i++
				i, redirects = readTarget(s, i+1, redirects, &opaque)
				break
			}
			flush()
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
			if i+1 < len(s) && s[i+1] == '&' { // 2>&1: a descriptor, not a file
				cur.WriteByte(c)
				i++
				cur.WriteByte('&')
				for i+1 < len(s) && (s[i+1] >= '0' && s[i+1] <= '9' || s[i+1] == '-') {
					i++
					cur.WriteByte(s[i])
				}
				break
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
