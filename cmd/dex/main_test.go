package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

	write(t, filepath.Join(proj, ".dex", "config.json"), `{"model":"from-project"}`)
	if s, err = loadSettings(proj, "", config.Flags{}); err != nil || s.Model != "from-project" || s.Provider != "anthropic" {
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
	if f, err := selectFront("repl", "", frontInfo{}); err != nil {
		t.Fatal(err)
	} else if _, ok := f.(*replFront); !ok {
		t.Fatalf("front is %T", f)
	}
	if f, _ := selectFront("repl", "hello", frontInfo{Prompt: "hello"}); f == nil {
		t.Fatal("-p selects print")
	} else if _, ok := f.(*printFront); !ok {
		t.Fatalf("front is %T", f)
	}
	if _, err := selectFront("tui", "", frontInfo{}); err == nil || !strings.Contains(err.Error(), "want repl") {
		t.Fatalf("err = %v", err)
	}
}
