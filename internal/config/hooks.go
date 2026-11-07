package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Hooks are the user's own hooks, in Claude Code's settings shape: by
// event, a list of matchers, each with the commands it runs. dax runs
// the two tool events, PreToolUse and PostToolUse (dax-hooks).
type Hooks struct {
	PreToolUse  []HookMatcher
	PostToolUse []HookMatcher
	// Dir is the directory of the config file that set the hooks, which
	// a program named by a relative path is resolved against; Resolve
	// sets it.
	Dir string `json:"-"`
}

// HookEvents are the events a hooks block may name.
var HookEvents = []string{"PreToolUse", "PostToolUse"}

// HookMatcher is the commands run for the calls whose tool's name
// Matcher matches: a regular expression over the whole name, dax's
// (bash, read, mcp__server__tool); "" and "*" match every tool.
type HookMatcher struct {
	Matcher string        `json:"matcher,omitempty"`
	Hooks   []HookCommand `json:"hooks"`
}

// HookCommand is one hook: a command run on this machine, given the
// call as JSON on its standard input, within Timeout seconds (0 is
// DefaultHookTimeout). A string is a shell's command line, run by
// /bin/sh -c as Claude Code runs a hook's; an array is the program and
// its arguments, run with no shell.
type HookCommand struct {
	// Type is "command", the one kind dax runs.
	Type    string  `json:"type"`
	Command Command `json:"command"`
	Timeout int     `json:"timeout,omitempty"`
}

// DefaultHookTimeout is a hook's time limit in seconds when it gives
// none, as Claude Code's.
const DefaultHookTimeout = 60

// maxHookTimeout is the longest a hook may be given.
const maxHookTimeout = 600

// UnmarshalJSON reads the events dax runs and refuses any other by
// name, so a block copied from Claude Code's settings says which of
// its events dax does not run rather than failing on a field.
func (h *Hooks) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("hooks: %w", err)
	}
	*h = Hooks{}
	for _, event := range sortedKeys(raw) {
		var dst *[]HookMatcher
		switch event {
		case "PreToolUse":
			dst = &h.PreToolUse
		case "PostToolUse":
			dst = &h.PostToolUse
		default:
			return fmt.Errorf("hooks: dax runs %s hooks; %q is not one", strings.Join(HookEvents, " and "), event)
		}
		dec := json.NewDecoder(bytes.NewReader(raw[event]))
		dec.DisallowUnknownFields()
		if err := dec.Decode(dst); err != nil {
			return fmt.Errorf("hooks.%s: %w", event, err)
		}
	}
	return nil
}

// MarshalJSON writes the events by name.
func (h Hooks) MarshalJSON() ([]byte, error) {
	out := map[string][]HookMatcher{}
	if len(h.PreToolUse) > 0 {
		out["PreToolUse"] = h.PreToolUse
	}
	if len(h.PostToolUse) > 0 {
		out["PostToolUse"] = h.PostToolUse
	}
	return json.Marshal(out)
}

// check refuses a matcher that is not a regular expression, a hook
// that is not a command, a command with no program and a timeout out of
// range.
func (h *Hooks) check() error {
	for _, ev := range []struct {
		name string
		ms   []HookMatcher
	}{{"PreToolUse", h.PreToolUse}, {"PostToolUse", h.PostToolUse}} {
		for i, m := range ev.ms {
			at := fmt.Sprintf("hooks.%s[%d]", ev.name, i)
			if _, err := HookPattern(m.Matcher); err != nil {
				return fmt.Errorf("%s.matcher %q: %w", at, m.Matcher, err)
			}
			if len(m.Hooks) == 0 {
				return fmt.Errorf("%s: no hooks", at)
			}
			for j, c := range m.Hooks {
				at := fmt.Sprintf("%s.hooks[%d]", at, j)
				if c.Type != "command" {
					return fmt.Errorf(`%s.type %q: dax runs "command" hooks`, at, c.Type)
				}
				if c.Command.array {
					if len(c.Command.argv) == 0 || c.Command.argv[0] == "" {
						return fmt.Errorf("%s.command: no program", at)
					}
				} else if strings.TrimSpace(c.Command.line) == "" {
					return fmt.Errorf("%s.command: no program", at)
				}
				if c.Timeout < 0 || c.Timeout > maxHookTimeout {
					return fmt.Errorf("%s.timeout %d: want 1 to %d seconds, or 0 for %d", at, c.Timeout, maxHookTimeout, DefaultHookTimeout)
				}
			}
		}
	}
	return nil
}

// HookPattern is a matcher as the expression it is: the whole tool
// name, so "bash" is not also "bash_output"; "" and "*" are every tool.
func HookPattern(matcher string) (*regexp.Regexp, error) {
	if matcher == "" || matcher == "*" {
		return regexp.MustCompile(`.*`), nil
	}
	re, err := regexp.Compile(`^(?:` + matcher + `)$`)
	if err != nil {
		return nil, fmt.Errorf("not a regular expression: %w", err)
	}
	return re, nil
}

// Empty says whether h runs nothing.
func (h *Hooks) Empty() bool {
	return h == nil || len(h.PreToolUse) == 0 && len(h.PostToolUse) == 0
}

// Program is the hook as it runs: shell, the line /bin/sh runs, for a
// string; argv, the program and its arguments, for an array. A program
// named by a relative path (one with a slash, "./guard", "hooks/x") is
// the one in dir, the user's config directory, never one in the
// directory the session runs in, which is the repository's: an
// array's first element is joined to dir, and a line whose first word
// is such a path, a plain word, has it replaced by dir's, quoted. An
// array's "~/" is the user's home; a line's ~ is the shell's.
func (c HookCommand) Program(dir string) (shell string, argv []string, err error) {
	resolve := func(p string) (string, bool, error) {
		switch {
		case strings.HasPrefix(p, "~/"):
			home, err := os.UserHomeDir()
			if err != nil {
				return "", false, fmt.Errorf("hook %q: %w", p, err)
			}
			return filepath.Join(home, p[2:]), true, nil
		case filepath.IsAbs(p) || !strings.Contains(p, "/"):
			return p, false, nil
		case !filepath.IsAbs(dir):
			return "", false, fmt.Errorf("hook %q: a relative program, and no config directory to find it in", p)
		}
		return filepath.Join(dir, p), true, nil
	}
	if c.Command.array {
		argv = slices.Clone(c.Command.argv)
		if len(argv) == 0 {
			return "", nil, errors.New("hook: no program")
		}
		if argv[0], _, err = resolve(argv[0]); err != nil {
			return "", nil, err
		}
		return "", argv, nil
	}
	line := strings.TrimLeft(c.Command.line, " \t")
	word, rest, _ := strings.Cut(line, " ")
	if strings.HasPrefix(word, "~") || !plainWord.MatchString(word) {
		return line, nil, nil
	}
	p, changed, err := resolve(word)
	if err != nil {
		return "", nil, err
	}
	if !changed {
		return line, nil, nil
	}
	if strings.Contains(p, "'") {
		return "", nil, fmt.Errorf("hook %q: its resolved path %s has a quote", word, p)
	}
	shell = "'" + p + "'"
	if rest != "" {
		shell += " " + rest
	}
	return shell, nil, nil
}

// plainWord is a shell word with nothing the shell reads in it, which
// may be replaced by another quoted.
var plainWord = regexp.MustCompile(`^[A-Za-z0-9._/+-]+$`)
