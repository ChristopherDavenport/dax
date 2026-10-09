package tool

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"strings"

	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agenttool"
)

// sentinel prefixes a subject no rule names, so a call that carries it
// is decided by a bare rule, the fallback or the user, and never by an
// allow rule for a command. See BashSubjects.
const sentinel = "$(...) "

// BashSubjects returns the subjects splitter of the bash tool for the
// workspace f. What it decides is who may auto-run:
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
// call, and so are arguments with a key that is command or dax_stamp
// in another case (exactKeys), which the tool would read and the
// analysis would not.
func BashSubjects(f *Files, maxFile int64) agentpolicy.Subjects {
	an := &Analyzer{Files: f, MaxFile: maxFile}
	return func(args json.RawMessage) ([]agentpolicy.Subject, error) {
		calls, _, err := bashFacts(context.Background(), an, args, false)
		if err != nil {
			return nil, err
		}
		return subjectsOf(calls), nil
	}
}

// bashFacts is what a bash call would touch: the calls BashSubjects
// describes, and, with stamp, the arguments the call runs with if the
// policy allows it unasked (StampArgs). One analysis serves both when
// the command is given without surrounding space, as the model writes
// it; otherwise the stamp is of the line exactly as given, which is
// what the tool checks again before it runs.
func bashFacts(ctx context.Context, an *Analyzer, args json.RawMessage, stamp bool) (calls []agenttool.FactCall, rewrite json.RawMessage, err error) {
	if err := exactKeys(args, "command", "dax_stamp"); err != nil {
		return nil, nil, err
	}
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, nil, err
	}
	mk := func(tool, field, match, text string) agenttool.FactCall {
		a, _ := json.Marshal(map[string]string{field: match})
		return agenttool.FactCall{Args: a, Tool: tool, Text: text}
	}
	cmd := strings.TrimSpace(in.Command)
	if cmd == "" {
		return nil, nil, errors.New("command is empty")
	}
	c := an.Check(ctx, cmd)
	// Never nil: a line that analyses to nothing is no calls, which a
	// policy refuses, and not the call itself, which a rule could allow.
	calls = []agenttool.FactCall{}
	if c.Parsed {
		for _, st := range c.Stages {
			calls = append(calls, mk("", "command", st.Match, st.Text))
			// What a stage reads is also a read of that path, so the
			// rules for secret-looking files and the user's own path
			// rules apply to cat, head, grep and git show as to read.
			for _, r := range st.Reads {
				calls = append(calls, mk("read", "path", r, st.Text+"  [reads "+r+"]"))
			}
			if st.Governed && !st.OK {
				calls = append(calls, mk("", "command", sentinel+st.Text, st.Text+"  ["+st.Why+"]"))
			}
		}
	} else {
		parts, targets, _, err := splitShell(cmd)
		if err != nil {
			return nil, nil, err
		}
		for _, p := range parts {
			calls = append(calls, mk("", "command", p, p))
		}
		for _, t := range targets {
			calls = append(calls, mk("write", "path", t, t))
		}
		calls = append(calls, mk("", "command", sentinel+cmd, cmd))
	}
	if !stamp {
		return calls, nil, nil
	}
	if in.Command != cmd {
		c = an.Check(ctx, in.Command)
	}
	rewrite, err = stampWith(c, args)
	return calls, rewrite, err
}

// subjectsOf is calls as agentpolicy's subjects, nil for nil.
func subjectsOf(calls []agenttool.FactCall) []agentpolicy.Subject {
	if calls == nil {
		return nil
	}
	out := make([]agentpolicy.Subject, 0, len(calls))
	for _, c := range calls {
		out = append(out, agentpolicy.Subject{Args: c.Args, Tool: c.Tool, Text: c.Text})
	}
	return out
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

// unresolvedTool is the tool a subject is decided as when the
// workspace cannot say where a path's links lead: no rule names it, so
// the call falls to the policy's default, which asks, whatever a bare
// rule for the file tool says. A link called notes.txt might be .env.
const unresolvedTool = "dax:links-unknown"

// PathSubjects returns the subjects splitter of a file tool: its one
// subject is the path in field, normalised against the workspace's root
// (see normalizePath), so a rule written for docs/** meets docs/../.git/x
// as .git/x, ./.env and a/../.env as .env and the absolute path of a
// file as the same name. The name the model used is matched, and so is
// what its links lead to, read through the workspace: a file called
// notes.txt that is a link to .env is .env to the rules. A workspace
// that cannot read links adds a subject no rule allows, so the call
// asks. A path that leaves the workspace gets a subject no rule names,
// so it asks. When the field is absent the subject is def, which is the
// directory a search defaults to. Arguments with a key that is field in
// another case are an error, which blocks the call (exactKeys): the
// tool would decode that key, and this reads field.
func PathSubjects(f *Files, field, def string) agentpolicy.Subjects {
	v := f.view()
	return func(args json.RawMessage) ([]agentpolicy.Subject, error) {
		calls, err := pathCalls(v, field, def, args)
		if err != nil {
			return nil, err
		}
		return subjectsOf(calls), nil
	}
}

// pathCalls is what a file tool's call would touch: the path in field,
// normalised, and what its links lead to (see PathSubjects).
func pathCalls(v view, field, def string, args json.RawMessage) ([]agenttool.FactCall, error) {
	if err := exactKeys(args, field); err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return nil, err
	}
	var raw string
	if val, ok := m[field]; ok {
		if err := json.Unmarshal(val, &raw); err != nil {
			return nil, err
		}
	}
	if raw == "" {
		raw = def
	}
	rel, ok := v.rel(raw)
	if !ok {
		rel = sentinel + raw
	}
	a, _ := json.Marshal(map[string]string{"path": rel})
	calls := []agenttool.FactCall{{Args: a, Text: raw}}
	if ok {
		switch target, r := v.resolve(rel); {
		case r == inside && target != rel:
			ta, _ := json.Marshal(map[string]string{"path": target})
			calls = append(calls, agenttool.FactCall{Args: ta, Text: raw + " -> " + target})
		case r == unknown:
			calls = append(calls, agenttool.FactCall{Args: a, Tool: unresolvedTool, Text: raw + " (where its links lead cannot be read)"})
		}
	}
	return calls, nil
}

// pathFacts is a file tool's facts claim over f: the path in field, or
// def when the call names none, and the arguments carrying the stamp of
// those facts (factsStamp), which the tool checks when it runs. A key
// that is field or dax_stamp in another case is an error (exactKeys).
func pathFacts(f *Files, name, field, def string) func(context.Context, json.RawMessage) (agenttool.Facts, error) {
	v := f.view()
	return func(_ context.Context, args json.RawMessage) (agenttool.Facts, error) {
		if err := exactKeys(args, "dax_stamp"); err != nil {
			return agenttool.Facts{}, err
		}
		calls, err := pathCalls(v, field, def, args)
		if err != nil {
			return agenttool.Facts{}, err
		}
		rewrite, err := withStamp(args, factsStamp(name, calls))
		return agenttool.Facts{Calls: calls, Rewrite: rewrite}, err
	}
}

// checkTouched is a file tool's side of its stamp: a call that carries
// one runs only if the facts of path, read now, are the facts it was
// allowed on, so a path that became a link to somewhere else since is
// refused (errTouched). A call with no stamp, one run with the policy
// off, is not checked. What is left is the moment between this reading
// and the tool's own open.
func (f *Files) checkTouched(name, def, path, stamp string) error {
	if stamp == "" {
		return nil
	}
	args := json.RawMessage(`{}`)
	if path != "" {
		args, _ = json.Marshal(map[string]string{"path": path})
	}
	calls, err := pathCalls(f.view(), "path", def, args)
	if err != nil || !hmac.Equal([]byte(factsStamp(name, calls)), []byte(stamp)) {
		return errTouched
	}
	return nil
}
