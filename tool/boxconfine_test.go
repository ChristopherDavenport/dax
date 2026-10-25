package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
	workspace "github.com/ChristopherDavenport/agentworkspace"
)

// confineFiles is a workspace with a repository's .git, a source file,
// and a link the repository already has into .git/hooks, which a write
// through it must be read as reaching.
func confineFiles(t *testing.T) *Files {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{".git/hooks", "src", ".github/workflows"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "src/a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".git/hooks", filepath.Join(dir, "hooks")); err != nil {
		t.Fatal(err)
	}
	l, err := workspace.NewLocal(dir, DefaultEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return NewFiles(l)
}

func factsOf(t *testing.T, tl agenttool.Tool, args string) agenttool.Facts {
	t.Helper()
	f, ok := tl.(agenttool.Factual)
	if !ok {
		t.Fatalf("%s makes no facts claim", tl.Name())
	}
	got, err := f.Facts(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("facts of %s: %v", args, err)
	}
	return got
}

// What box.confine "workspace" may take as confined of a bash line:
// one the analyzer reads whole, whose text hides nothing and names no
// protected path, the root, a parent or a path outside, and that runs
// no program that runs another.
func TestConfinableBash(t *testing.T) {
	f := confineFiles(t)
	bash := Bash(f)
	for _, tc := range []struct {
		line string
		want bool
	}{
		// Lines the analyzer reads that touch only the project's source.
		{"ls src", true},
		{"go test ./...", true},
		{"go vet ./... && go build ./...", true},
		{"rm -rf src", true},
		{"mkdir -p build && cp src/a.go build/", true},
		{"make", true},
		{"npm test", true},
		// git writes .git, so no git line is confinable; the ones the
		// policy allows anyway (git status) run as they do without a
		// box. A leading assignment is not read by the analyzer, so the
		// line asks as without a box.
		{"git status", false},
		{"GOFLAGS=-mod=mod go test ./...", false},
		// Protected paths, however the line spells them.
		{"rm -rf .git", false},
		{"mv x .git/hooks/pre-commit", false},
		{"sed -i s/a/b/ .git/config", false},
		{"cp evil .github/workflows/ci.yml", false},
		{"touch .envrc", false},
		{"echo x > .env.local", false},
		{"cat x >> .dax/config.json", false},
		{"touch '.g'it/x", false},
		{`touch ".git"/x`, false},
		{"touch src/.git/x", false},
		{"go build -o=.git/x ./...", false},
		{"cp a.yml .gitlab-ci.yml", false},
		{"touch .VSCODE/tasks.json", false},
		// A redirect into a protected path, links followed: hooks is a
		// link to .git/hooks.
		{"echo x > hooks/pre-commit", false},
		// Text that can build a name the line does not show.
		{"sh -c 'echo x > .git/hooks/pre-commit'", false},
		{"bash -c 'rm -rf src'", false},
		{"eval rm -rf src", false},
		{"rm -rf $(echo .gi)t", false},
		{"rm -rf `echo .git`", false},
		{"x=.git; rm -rf $x", false},
		{`rm -rf .g\it`, false},
		{"python3 -c 'open(1)'", false},
		{"env rm -rf src", false},
		{"xargs rm < list", false},
		{"find src -delete", false},
		{"git commit -m x", false},
		{"ln -s .git/hooks h", false},
		// The root, a parent, outside, home, dot globs.
		{"rm -rf .", false},
		{"chmod -R 777 ./", false},
		{"rm -rf ../other", false},
		{"rm -rf /tmp/x", false},
		{"rm -rf ~/x", false},
		{"rm -rf .g*", false},
		{"rm -rf src/.g?t", false},
		{"cat a b | tee ../x", false},
	} {
		args := mustJSON(t, map[string]string{"command": tc.line})
		got := ConfinableBash(args, factsOf(t, bash, string(args)))
		if got != tc.want {
			t.Errorf("ConfinableBash(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

// What box.confine "workspace" may take as confined of a file tool's
// call: a path inside the project, links followed, off the protected
// paths.
func TestConfinablePath(t *testing.T) {
	f := confineFiles(t)
	for _, tc := range []struct {
		tool agenttool.Tool
		path string
		want bool
	}{
		{Write(f), "src/a.go", true},
		{Write(f), "src/new/b.go", true},
		{Edit(f), "src/a.go", true},
		{Write(f), "Makefile", true},
		{Write(f), ".git/config", false},
		{Write(f), ".git/hooks/pre-commit", false},
		{Edit(f), ".github/workflows/ci.yml", false},
		{Write(f), ".envrc", false},
		{Write(f), ".env", false},
		{Write(f), "sub/.env.local", false},
		{Write(f), ".dax/config.json", false},
		{Write(f), ".pre-commit-config.yaml", false},
		{Write(f), ".husky/pre-push", false},
		{Write(f), ".idea/workspace.xml", false},
		{Write(f), ".GIT/config", false},
		// Through the link the repository has into .git/hooks.
		{Write(f), "hooks/pre-commit", false},
		// Outside the root.
		{Write(f), "../x", false},
		{Write(f), "/etc/passwd", false},
	} {
		args := mustJSON(t, map[string]string{"path": tc.path, "content": "x"})
		if tc.tool.Name() == "edit" {
			args = mustJSON(t, map[string]string{"path": tc.path, "old": "package", "new": "package"})
		}
		fa, ok := tc.tool.(agenttool.Factual)
		if !ok {
			t.Fatalf("%s makes no claim", tc.tool.Name())
		}
		facts, err := fa.Facts(context.Background(), args)
		if err != nil {
			// A claim that refuses the call (a path outside) blocks it;
			// it is not confinable.
			if tc.want {
				t.Errorf("%s %s: facts: %v", tc.tool.Name(), tc.path, err)
			}
			continue
		}
		if got := ConfinablePath(facts); got != tc.want {
			t.Errorf("ConfinablePath(%s %s) = %v, want %v (facts %+v)", tc.tool.Name(), tc.path, got, tc.want, facts.Calls)
		}
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
