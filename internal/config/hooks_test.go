package config

import (
	"strings"
	"testing"
)

// The hooks block is Claude Code's shape, read from the user's file
// alone: a repository cannot add a command that runs (#148).
func TestHooksAreTheUsersAlone(t *testing.T) {
	block := `{"hooks":{"PreToolUse":[{"matcher":"bash","hooks":[{"type":"command","command":"guard --strict","timeout":5}]}],
		"PostToolUse":[{"matcher":"write|edit","hooks":[{"type":"command","command":["gofmt","-l","."]}]}]}}`
	l, err := Parse([]byte(block), "/u/config.json", false)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Resolve([]Layer{l}, Flags{}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := s.Hooks
	if h == nil || len(h.PreToolUse) != 1 || len(h.PostToolUse) != 1 || h.PreToolUse[0].Hooks[0].Timeout != 5 {
		t.Fatalf("hooks %+v", h)
	}
	if argv, _ := h.PreToolUse[0].Hooks[0].Command.Argv(); strings.Join(argv, " ") != "guard --strict" {
		t.Errorf("command %q", argv)
	}
	re, _ := HookPattern(h.PostToolUse[0].Matcher)
	if !re.MatchString("edit") || re.MatchString("edit_notebook") {
		t.Errorf("matcher %q matches edit %v, edit_notebook %v; want the whole name", re, re.MatchString("edit"), re.MatchString("edit_notebook"))
	}
	if _, err := Parse([]byte(block), "/p/.dax/config.json", true); err == nil || !strings.Contains(err.Error(), "hooks: a project file may only tighten") {
		t.Fatalf("a project's hooks: %v", err)
	}
	if s, _ := Resolve(nil, Flags{}, ""); s.Hooks != nil {
		t.Errorf("no hooks block resolved to %+v", s.Hooks)
	}
}

func TestABadHooksBlockFailsWithThePath(t *testing.T) {
	for _, tc := range []struct{ name, json, want string }{
		{"event dax does not run", `{"hooks":{"Stop":[]}}`, `dax runs PreToolUse and PostToolUse hooks; "Stop" is not one`},
		{"not a command", `{"hooks":{"PreToolUse":[{"hooks":[{"type":"prompt","command":"x"}]}]}}`, `hooks.PreToolUse[0].hooks[0].type "prompt"`},
		{"no program", `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":""}]}]}}`, `hooks.PreToolUse[0].hooks[0].command: no program`},
		{"bad matcher", `{"hooks":{"PostToolUse":[{"matcher":"(","hooks":[{"type":"command","command":"x"}]}]}}`, `hooks.PostToolUse[0].matcher "("`},
		{"bad timeout", `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"x","timeout":9999}]}]}}`, `timeout 9999`},
		{"no hooks", `{"hooks":{"PreToolUse":[{"matcher":"bash"}]}}`, `hooks.PreToolUse[0]: no hooks`},
		{"unknown field", `{"hooks":{"PreToolUse":[{"matcher":"bash","when":"always","hooks":[]}]}}`, `unknown field`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.json), "/u/config.json", false)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "/u/config.json") {
				t.Fatalf("err %v, want %q with the path", err, tc.want)
			}
		})
	}
}
