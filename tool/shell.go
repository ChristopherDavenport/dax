package tool

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
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
//     ever covers it. A redirect to a file is a write of the file it
//     opens, links followed (redirectCalls). The cut is for the question
//     the user is asked and for deny and ask rules; it is a best-effort
//     reading of bash, not a parser, and nothing is allowed because of
//     it.
//
// A command with an unterminated quote is an error, which blocks the
// call, and so are arguments with a key that is command or dax_stamp
// in another case (exactKeys), which the tool would read and the
// analysis would not. The analysis runs under the context the policy
// passes, the decision's, so the git-config check it runs through the
// workspace's Exec is cancelled with the decision.
func BashSubjects(f *Files, maxFile int64) agentpolicy.Subjects {
	an := &Analyzer{Files: f, MaxFile: maxFile}
	return func(ctx context.Context, args json.RawMessage) ([]agentpolicy.Subject, error) {
		calls, _, err := bashFacts(ctx, an, args, false)
		if err != nil {
			return nil, err
		}
		return subjectsOf(calls), nil
	}
}

// bashFacts is what a bash call would touch: the calls BashSubjects
// describes, and, with stamp, the arguments the call runs with if the
// policy lets it run (stampWith): an auto-allowed line's plan stamp
// (StampArgs), or, for a line outside the safe subset that writes
// through a redirect, the stamp of the line and of where its redirects
// lead (lineStamp). One analysis serves both, so the stamp binds the
// facts the policy decides on: a second look could find a path that
// became a link in between. The tool analyses the line exactly as
// given, which Check trims of spaces and tabs only; a line with other
// space at its ends (a newline) is outside the safe subset as given, so
// it is never plan-stamped, whatever its trimmed form is.
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
	calls, c, writes, err := bashCalls(ctx, an, in.Command)
	if err != nil || !stamp {
		return calls, nil, err
	}
	if strings.Trim(in.Command, " \t") != strings.TrimSpace(in.Command) {
		c = &Check{}
	}
	line := ""
	if writes {
		line = lineStamp(in.Command, calls)
	}
	rewrite, err = stampWith(c, line, args)
	return calls, rewrite, err
}

// bashCalls is the claim's calls for the bash line command, trimmed, and
// the analysis they come from. A line in the safe subset is its stages
// (parsedCalls). Any other line is the parts splitShell cuts it into,
// what its redirects write (redirectCalls) and a subject no rule names;
// writes says it has a redirect to a file, which makes its calls depend
// on where the links on the way lead.
func bashCalls(ctx context.Context, an *Analyzer, command string) (calls []agenttool.FactCall, c *Check, writes bool, err error) {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return nil, nil, false, errors.New("command is empty")
	}
	c = an.Check(ctx, cmd)
	if c.Parsed {
		return parsedCalls(c), c, false, nil
	}
	parts, targets, _, err := splitShell(cmd)
	if err != nil {
		return nil, nil, false, err
	}
	calls = []agenttool.FactCall{}
	for _, p := range parts {
		calls = append(calls, mkCall("", "command", p, p))
	}
	calls = append(calls, redirectCalls(an.Files.view(), parts, targets)...)
	calls = append(calls, mkCall("", "command", sentinel+cmd, cmd))
	return calls, c, len(targets) > 0, nil
}

func mkCall(tool, field, match, text string) agenttool.FactCall {
	a, _ := json.Marshal(map[string]string{field: match})
	return agenttool.FactCall{Args: a, Tool: tool, Text: text}
}

// redirectCalls is what the redirects of a line outside the safe subset
// write, each decided as the write tool's call on the file it opens, as
// a file tool's path is (pathCalls): the name, normalised against the
// workspace's root from the directory the line is in when it opens it,
// and what the links on its way lead to, read through the workspace, so
// `echo x > notes` is a write of .env when notes is a link to it.
//
// splitShell is a reader and not a shell, so where the line is is not
// known for certain: a target is read from the root and from each
// directory a plain `cd DIR` before it leads to, and is a write of
// every name that gives. A cd in a pipe or a subshell may not hold, and
// one with anything to expand is not followed; reading the root too
// keeps the name the target had before any cd. A target bash expands
// ($, a glob, ~) or one outside the workspace is its text, as it was. A
// target whose links lead out of the workspace adds a write no rule
// names; one whose links the workspace cannot read adds a subject no
// rule names (unresolvedTool), so the call asks.
func redirectCalls(v view, parts []string, targets []target) []agenttool.FactCall {
	var calls []agenttool.FactCall
	seen := map[string]bool{}
	add := func(tool, p, text string) {
		c := mkCall(tool, "path", p, text)
		if k := tool + "\x00" + string(c.Args); !seen[k] {
			seen[k] = true
			calls = append(calls, c)
		}
	}
	for _, t := range targets {
		if strings.HasPrefix(t.text, "~") || strings.ContainsAny(t.text, "$`*?\\") {
			add("write", t.text, t.text)
			continue
		}
		for _, cwd := range cdDirs(v.dir, parts[:t.part]) {
			text := t.text
			if crel, ok := v.rel(cwd); ok && crel != "." && !filepath.IsAbs(t.text) {
				text += " (from " + crel + ")"
			}
			name := t.text
			if !filepath.IsAbs(name) {
				name = filepath.Join(cwd, name)
			}
			rel, ok := v.rel(name)
			if ok {
				add("write", rel, text)
			} else {
				add("write", t.text, text)
			}
			switch to, r := v.reach(cwd, t.text); {
			case r == inside && (!ok || to != rel):
				add("write", to, text+" -> "+to)
			case r == outside && ok:
				add("write", sentinel+t.text, text+"  [a link on its way leads out of the workspace]")
			case r == unknown:
				if !ok {
					rel = t.text
				}
				add(unresolvedTool, rel, text+"  [where its links lead cannot be read]")
			}
		}
	}
	return calls
}

// reach is where a redirect to p in the directory cwd, absolute in the
// workspace's namespace, opens its file: from where cwd really is, its
// links followed, then p's names one at a time, a ".." from where the
// name before it leads, as the kernel takes them.
func (v view) reach(cwd, p string) (string, reach) {
	if filepath.IsAbs(p) {
		for _, base := range []string{v.dir, v.real} {
			if base == "" {
				continue
			}
			if p == base || strings.HasPrefix(p, strings.TrimSuffix(base, string(filepath.Separator))+string(filepath.Separator)) {
				return v.resolve(strings.TrimPrefix(p, base))
			}
		}
		return "", outside
	}
	crel, ok := v.rel(cwd)
	if !ok {
		return "", outside
	}
	dir, r := v.resolve(crel)
	if r != inside {
		return "", r
	}
	return v.resolve(filepath.ToSlash(dir) + "/" + filepath.ToSlash(p))
}

// cdDirs are the directories, absolute in the workspace's namespace, a
// line may be in after parts: root, and each one a part that is a plain
// `cd DIR` leads to from the one before, as bash's cd takes DIR, with
// its ".." taken off the names before it. A cd whose directory has
// anything bash expands or quotes is not followed.
func cdDirs(root string, parts []string) []string {
	dirs := []string{root}
	cur := root
	for _, p := range parts {
		f := strings.Fields(strings.TrimLeft(p, "({ \t"))
		for len(f) > 0 && (f[0] == "then" || f[0] == "do" || f[0] == "else") {
			f = f[1:]
		}
		if len(f) != 2 || f[0] != "cd" || strings.HasPrefix(f[1], "-") || strings.ContainsAny(f[1], "$`'\"\\~*?[]{}()") {
			continue
		}
		if filepath.IsAbs(f[1]) {
			cur = filepath.Clean(f[1])
		} else {
			cur = filepath.Join(cur, f[1])
		}
		if !slices.Contains(dirs, cur) {
			dirs = append(dirs, cur)
		}
	}
	return dirs
}

// parsedCalls is what a line in the safe subset would touch, from its
// analysis c: each stage as a command, what it reads (a read of the
// path and of where its links lead) and, for a governed stage whose
// arguments fail or a git whose configuration names a program, a
// subject no rule names. It is the claim's calls and what an
// auto-allowed line's stamp signs (stampOfCheck). Never nil: a line
// that analyses to nothing is no calls, which a policy refuses, and not
// the call itself, which a rule could allow.
func parsedCalls(c *Check) []agenttool.FactCall {
	mk := mkCall
	calls := []agenttool.FactCall{}
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
	return calls
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
func splitShell(s string) (parts []string, redirects []target, opaque bool, err error) {
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
				i, redirects = readTarget(s, i+1, len(parts), redirects, &opaque)
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
			i, redirects = readTarget(s, i+1, len(parts), redirects, &opaque)
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

// target is a redirect's target as splitShell read it: its text, the
// quotes at its ends taken off, and the number of parts before the one
// it belongs to, which says which cds came before it.
type target struct {
	text string
	part int
}

// readTarget reads the word after a redirect operator at s[i:], records
// it unless it is /dev/null, and returns the index of its last byte.
// part is the number of parts cut before it.
func readTarget(s string, i, part int, redirects []target, opaque *bool) (int, []target) {
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
	text := strings.Trim(s[start:i], `'"`)
	switch {
	case text == "":
		*opaque = true
	case text == "/dev/null":
	case strings.ContainsAny(text, "$`*?"):
		*opaque = true
		redirects = append(redirects, target{text, part})
	default:
		redirects = append(redirects, target{text, part})
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
	return func(_ context.Context, args json.RawMessage) ([]agentpolicy.Subject, error) {
		calls, err := pathCalls(v, field, def, args)
		if err != nil {
			return nil, err
		}
		return subjectsOf(calls), nil
	}
}

// PathCalls is what a read of name, a path as a file tool takes it,
// would touch, as dax-coding's file tools claim it: name normalised
// against the workspace's root, and what its links lead to, read
// through f's workspace. The calls name no tool (Tool is empty, the
// tool that reads), except the one a workspace that cannot read links
// adds, which names a tool no rule names, so it asks. It is exported
// for an extension whose tool reads a workspace file by another name,
// dax-skills' skill tool reading a file of a project's skill, so its
// facts claim can name the file as a read would (see
// extension.Extension.HeldTo).
func PathCalls(f *Files, name string) ([]agenttool.FactCall, error) {
	args, err := json.Marshal(map[string]string{"path": name})
	if err != nil {
		return nil, err
	}
	return pathCalls(f.view(), "path", "", args)
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
