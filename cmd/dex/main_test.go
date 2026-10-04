package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dex/internal/config"
	"github.com/ChristopherDavenport/dex/internal/provider"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// home isolates the user's config and data directories.
func home(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(h, ".config"))
	return h
}

func str(s string) *string { return &s }

func TestSettingsPrecedenceFromRealFiles(t *testing.T) {
	h := home(t)
	proj := t.TempDir()
	write(t, filepath.Join(h, ".config", "dex", "config.json"), `{"provider":"anthropic","model":"from-user","think":false}`)

	s, err := loadSettings(proj, "", config.Flags{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Provider != "anthropic" || s.Model != "from-user" || s.Think {
		t.Fatalf("user file: %+v", s)
	}

	// A project file tightens the policy and cannot set the model.
	write(t, filepath.Join(proj, ".dex", "config.json"), `{"model":"from-project"}`)
	if _, err = loadSettings(proj, "", config.Flags{}); err == nil || !strings.Contains(err.Error(), "model: a project file may only tighten") {
		t.Fatalf("project model: %v", err)
	}
	write(t, filepath.Join(proj, ".dex", "config.json"), `{"policy":{"deny":["bash(git push:*)"]}}`)
	if s, err = loadSettings(proj, "", config.Flags{}); err != nil || s.Model != "from-user" || len(s.Policy.Project.Deny) != 1 {
		t.Fatalf("project over user: %+v, %v", s, err)
	}

	s, err = loadSettings(proj, "", config.Flags{Model: str("from-flag"), Provider: str("openai")})
	if err != nil || s.Model != "from-flag" || s.Provider != "openai" {
		t.Fatalf("flags over files: %+v, %v", s, err)
	}
	if s.MemoryDir != filepath.Join(h, ".dex", "memory") {
		t.Errorf("default memory dir = %q", s.MemoryDir)
	}

	// -config names another user file, which must exist.
	other := filepath.Join(h, "other.json")
	if _, err := loadSettings(proj, other, config.Flags{}); err == nil {
		t.Error("a missing -config file should be an error")
	}
	write(t, other, `{"provider":"gemini"}`)
	if s, err = loadSettings(proj, other, config.Flags{}); err != nil || s.Provider != "gemini" {
		t.Fatalf("-config: %+v, %v", s, err)
	}
}

func TestABrokenConfigFileIsAClearError(t *testing.T) {
	h := home(t)
	path := filepath.Join(h, ".config", "dex", "config.json")
	write(t, path, `{"provider":"ollama","modle":"x"}`)
	_, err := loadSettings(t.TempDir(), "", config.Flags{})
	if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "modle") {
		t.Fatalf("err = %v, want the file and the field named", err)
	}
}

func TestPricingParsesAndPrices(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.json")
	write(t, path, `{"m":{"input":2,"cached":0.5,"output":8}}`)
	cost, err := pricing(path)
	if err != nil {
		t.Fatal(err)
	}
	u := openresponses.Usage{
		InputTokens:        3,
		OutputTokens:       2,
		InputTokensDetails: openresponses.InputTokensDetails{CachedTokens: 1},
	}
	got, ok := cost("m", u)
	if !ok || got != 0.0000205 {
		t.Fatalf("cost = %v, %v; want $0.0000205, true", got, ok)
	}
	if _, ok := cost("unknown", u); ok {
		t.Error("an unpriced model was shown as priced")
	}
	if _, err := pricing(""); err != nil {
		t.Errorf("empty pricing file: %v", err)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	write(t, bad, `[]`)
	if _, err := pricing(bad); err == nil || !strings.Contains(err.Error(), "pricing_file") {
		t.Fatalf("bad pricing file: %v", err)
	}
}

func TestSelectedProviderWithoutItsKeyFailsBeforeAnyRequest(t *testing.T) {
	home(t)
	t.Setenv("OPENAI_API_KEY", "")
	s, err := loadSettings(t.TempDir(), "", config.Flags{Provider: str("openai")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.New(context.Background(), provider.Spec{Provider: s.Provider, Model: s.Model, BaseURL: s.BaseURL})
	if err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Fatalf("err = %v", err)
	}
}

func TestFronts(t *testing.T) {
	none := frontEnv{recording: true}
	both := frontEnv{stdinTTY: true, stdoutTTY: true, recording: true}
	kind := func(f front) string {
		switch f.(type) {
		case *replFront:
			return "repl"
		case *printFront:
			return "print"
		case *tuiFront:
			return "tui"
		}
		return "?"
	}
	for _, tc := range []struct {
		name, prompt string
		env          frontEnv
		want         string
		wantErr      string
	}{
		{"repl", "", none, "repl", ""},
		{"repl", "", both, "repl", ""},
		{"tui", "", both, "tui", ""},
		{"", "", both, "tui", ""},
		{"", "", none, "repl", ""},
		{"", "", frontEnv{stdinTTY: true, recording: true}, "repl", ""},
		{"", "", frontEnv{stdoutTTY: true, recording: true}, "repl", ""},
		// -p wins over everything.
		{"repl", "hello", none, "print", ""},
		{"tui", "hello", both, "print", ""},
		{"", "hello", both, "print", ""},
		// No session store: the default is the REPL, and asking for the
		// terminal client by name is refused plainly.
		{"", "", frontEnv{stdinTTY: true, stdoutTTY: true}, "repl", ""},
		{"tui", "", frontEnv{stdinTTY: true, stdoutTTY: true}, "", `the terminal client needs a session store; drop -sessions "" or use -front repl`},
		{"repl", "", frontEnv{stdinTTY: true, stdoutTTY: true}, "repl", ""},
		// The terminal client needs a terminal at both ends.
		{"tui", "", none, "", "needs a terminal"},
		{"tui", "", frontEnv{stdinTTY: true, recording: true}, "", "needs a terminal"},
		{"tui", "", frontEnv{stdoutTTY: true, recording: true}, "", "needs a terminal"},
		{"gui", "", both, "", "want tui or repl"},
	} {
		f, err := selectFront(tc.name, tc.prompt, frontInfo{Prompt: tc.prompt}, tc.env)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("selectFront(%q, %q, %+v) err = %v, want containing %q", tc.name, tc.prompt, tc.env, err, tc.wantErr)
			}
			continue
		}
		if err != nil || kind(f) != tc.want {
			t.Errorf("selectFront(%q, %q, %+v) = %T, %v; want %s", tc.name, tc.prompt, tc.env, f, err, tc.want)
		}
	}
	// The pause waits on the input for a prompt written to the output, so
	// it exists only when both are terminals.
	f, _ := selectFront("tui", "", frontInfo{}, both)
	if !f.(*tuiFront).pause {
		t.Error("the terminal client does not pause with both ends a terminal")
	}
}

// A character device is not a terminal: /dev/null has the mode and
// not the line discipline.
func TestIsTerminalIsNotJustACharacterDevice(t *testing.T) {
	null, err := os.Open("/dev/null")
	if err != nil {
		t.Skip("no /dev/null")
	}
	defer null.Close()
	if fi, _ := null.Stat(); fi.Mode()&os.ModeCharDevice == 0 {
		t.Skip("/dev/null is not a character device here")
	}
	if isTerminal(null) {
		t.Error("/dev/null is a terminal")
	}
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	if isTerminal(r) || isTerminal(w) {
		t.Error("a pipe is a terminal")
	}
	if got := processEnv(true); got.stdinTTY && got.stdoutTTY && !isTerminal(os.Stdin) {
		t.Error("processEnv disagrees with isTerminal")
	}
}
