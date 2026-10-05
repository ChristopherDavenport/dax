package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tree writes files, name to content, under a fresh directory.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

var project = map[string]string{
	"main.go":              "package main\n\nfunc main() {}\n",
	"README.md":            "# hi\nTODO: docs\n",
	"cmd/dax/main.go":      "package main\n// TODO: flags\n",
	"internal/a/a.go":      "package a\n",
	"internal/a/a_test.go": "package a\n// todo lower\n",
	"internal/b/deep/b.go": "package b\n",
	"vendor/x/x.go":        "package x // TODO vendored\n",
	"node_modules/y/y.js":  "TODO\n",
	".git/config":          "TODO\n",
	"bin/blob":             "TODO\x00binary\n",
}

func TestGlob(t *testing.T) {
	dir := tree(t, project)
	ws := newWS(t, dir)
	tests := []struct {
		name string
		args string
		want string
	}{
		{"recursive", `{"pattern":"**/*.go"}`, "cmd/dax/main.go\ninternal/a/a.go\ninternal/a/a_test.go\ninternal/b/deep/b.go\nmain.go"},
		{"top level only", `{"pattern":"*.go"}`, "main.go"},
		{"zero dirs for **", `{"pattern":"internal/**/a.go"}`, "internal/a/a.go"},
		{"braces", `{"pattern":"**/*.{md,js}"}`, "README.md"},
		{"single segment star", `{"pattern":"cmd/*/main.go"}`, "cmd/dax/main.go"},
		{"path is the base", `{"pattern":"**/*.go","path":"internal"}`, "internal/a/a.go\ninternal/a/a_test.go\ninternal/b/deep/b.go"},
		{"pattern relative to path", `{"pattern":"*.go","path":"internal/a"}`, "internal/a/a.go\ninternal/a/a_test.go"},
		{"vendored dir named explicitly", `{"pattern":"**/*.go","path":"vendor"}`, "vendor/x/x.go"},
		{"limit", `{"pattern":"**/*.go","max_results":2}`, "... (stopped at 2 results"},
		{"none", `{"pattern":"**/*.rs"}`, "(no files match)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := call(context.Background(), Glob(ws), tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("got\n%s\nwant containing\n%s", got, tc.want)
			}
			for _, bad := range []string{".git/", "node_modules", "vendor/x"} {
				if tc.name != "vendored dir named explicitly" && strings.Contains(got, bad) {
					t.Errorf("result includes skipped %q:\n%s", bad, got)
				}
			}
		})
	}
	t.Run("exact output is sorted", func(t *testing.T) {
		got, _ := call(context.Background(), Glob(ws), `{"pattern":"**/*.go"}`)
		if want := tests[0].want; got != want {
			t.Fatalf("got\n%s\nwant\n%s", got, want)
		}
	})
	t.Run("errors", func(t *testing.T) {
		for args, want := range map[string]string{
			`{"pattern":""}`:                "pattern is required",
			`{"pattern":"[a"}`:              "bad pattern",
			`{"pattern":"{a,b"}`:            "unclosed",
			`{"pattern":"*","path":"../"}`:  "outside the workspace",
			`{"pattern":"*","path":"nope"}`: "no such file",
		} {
			if _, err := call(context.Background(), Glob(ws), args); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s: err = %v, want %q", args, err, want)
			}
		}
	})
}

func TestGrep(t *testing.T) {
	ws := newWS(t, tree(t, project))
	tests := []struct {
		name    string
		args    string
		want    []string
		notWant []string
	}{
		{"line numbers and paths", `{"pattern":"TODO"}`, []string{"README.md:2:TODO: docs", "cmd/dax/main.go:2:// TODO: flags"}, []string{"vendor", "node_modules", ".git", "bin/blob", "lower"}},
		{"ignore case", `{"pattern":"todo","ignore_case":true}`, []string{"README.md:2:", "internal/a/a_test.go:2:// todo lower"}, nil},
		{"case sensitive by default", `{"pattern":"todo"}`, []string{"internal/a/a_test.go:2:// todo lower"}, []string{"README.md"}},
		{"include by name", `{"pattern":"package","include":"*_test.go"}`, []string{"internal/a/a_test.go:1:package a"}, []string{"main.go"}},
		{"include by path glob", `{"pattern":"package","include":"internal/**/*.go"}`, []string{"internal/b/deep/b.go:1:package b"}, []string{"cmd/"}},
		{"path directory", `{"pattern":"package","path":"cmd"}`, []string{"cmd/dax/main.go:1:package main"}, []string{"internal"}},
		{"path file", `{"pattern":"main","path":"main.go"}`, []string{"main.go:1:package main", "main.go:3:func main"}, nil},
		{"regexp", `{"pattern":"^func \\w+\\(\\)"}`, []string{"main.go:3:func main() {}"}, nil},
		{"max results", `{"pattern":"package","max_results":2}`, []string{"... (stopped at 2 matches"}, nil},
		{"no matches", `{"pattern":"zzzz"}`, []string{"(no matches)"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := call(context.Background(), Grep(ws), tc.args)
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in\n%s", w, got)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(got, w) {
					t.Errorf("unexpected %q in\n%s", w, got)
				}
			}
		})
	}
	t.Run("max results counts lines", func(t *testing.T) {
		got, _ := call(context.Background(), Grep(ws), `{"pattern":"package","max_results":2}`)
		if n := strings.Count(got, "\n"); n != 2 { // two matches and the note
			t.Fatalf("got %d newlines:\n%s", n, got)
		}
	})
	t.Run("errors", func(t *testing.T) {
		for args, want := range map[string]string{
			`{"pattern":""}`:                   "pattern is required",
			`{"pattern":"("}`:                  "bad pattern",
			`{"pattern":"x","path":"/etc"}`:    "outside the workspace",
			`{"pattern":"x","path":"missing"}`: "no such file",
		} {
			if _, err := call(context.Background(), Grep(ws), args); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s: err = %v, want %q", args, err, want)
			}
		}
	})
}

func TestLS(t *testing.T) {
	ws := newWS(t, tree(t, project))
	got, err := call(context.Background(), LS(ws), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{".git/\n", "README.md  ", "bin/\n", "cmd/\n", "internal/\n", "main.go  29", "node_modules/\n", "vendor/"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Index(got, ".git/") > strings.Index(got, "README.md") || strings.Index(got, "README.md") > strings.Index(got, "main.go") {
		t.Errorf("not sorted:\n%s", got)
	}
	sub, err := call(context.Background(), LS(ws), `{"path":"internal/a"}`)
	if err != nil || sub != "a.go  10\na_test.go  24" {
		t.Errorf("sub = %q, %v", sub, err)
	}
	for args, want := range map[string]string{
		`{"path":"main.go"}`: "not a directory",
		`{"path":"nope"}`:    "no such file",
		`{"path":"/"}`:       "outside the workspace",
	} {
		if _, err := call(context.Background(), LS(ws), args); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", args, err, want)
		}
	}
	empty := newWS(t, t.TempDir())
	if got, _ := call(context.Background(), LS(empty), `{}`); got != "(empty)" {
		t.Errorf("empty dir: %q", got)
	}
}

func TestMatchGlob(t *testing.T) {
	for _, tc := range []struct {
		pat, name string
		want      bool
	}{
		{"**", "a/b/c", true},
		{"**/c", "c", true},
		{"**/c", "a/b/c", true},
		{"a/**", "a", true},
		{"a/**/c", "a/c", true},
		{"a/**/c", "a/b/b/c", true},
		{"a/**/c", "b/c", false},
		{"*.go", "a/b.go", false},
		{"a/*", "a/b/c", false},
		{"**/**/x", "x", true},
		{"a?c", "abc", true},
		{"[ab]x", "bx", true},
	} {
		if got := matchGlob(tc.pat, tc.name); got != tc.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tc.pat, tc.name, got, tc.want)
		}
	}
}
