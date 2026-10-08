package tool

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repoWithFiles is a git repository with a commit and a few files.
func repoWithFiles(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	for name, content := range map[string]string{"main.go": "package main\n// hello\n", "util.go": "package main\n", "sub/a.go": "package sub\n", "README.md": "# hi\nhello world\n"} {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
	}
	run("init", "-q")
	run("add", ".")
	run("-c", "user.name=n", "-c", "user.email=e@x", "commit", "-qm", "first")
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n// hello\n// more\n"), 0o644)
	run("-c", "user.name=n", "-c", "user.email=e@x", "commit", "-qam", "second")
	return dir
}

// The lines dax runs without asking do what bash would have done with
// them: the plan it parsed is the plan it renders.
func TestAutoAllowedLinesRunAsBashWouldRunThem(t *testing.T) {
	dir := repoWithFiles(t)
	an := &Analyzer{Files: newWS(t, dir)}
	b := Bash(newWS(t, dir))
	for cmd, want := range map[string][]string{
		"git log --oneline | head -n 1":              {"second", "[exit 0]"},
		"git log --oneline | wc -l":                  {"2\n"},
		"git log -n 1 --format=%s":                   {"second\n"},
		"git status -sb":                             {"## "},
		"git log --oneline 2>&1 | grep first":        {"first"},
		"git diff HEAD~1 --stat | tail -n 1":         {"1 file changed"},
		"cd sub && ls":                               {"a.go\n"},
		"cd sub && pwd":                              {"/sub\n"},
		"cd sub && git status -sb":                   {"## "},
		"ls *.go":                                    {"main.go\nutil.go\n"},
		"ls -d */":                                   {"sub/\n"},
		"ls sub/*.go":                                {"sub/a.go\n"},
		"cat util.go":                                {"package main\n"},
		"head -n 1 main.go":                          {"package main\n"},
		"tail -n 1 main.go":                          {"// more\n"},
		"wc -l main.go util.go":                      {"3 main.go", "1 util.go", "4 total"},
		"grep hello README.md main.go":               {"README.md:hello world", "main.go:// hello"},
		"grep -c package sub/a.go":                   {"1\n"},
		"git branch --list":                          {"* "},
		"git rev-parse --abbrev-ref HEAD":            {"[exit 0]"},
		"git ls-files sub":                           {"sub/a.go\n"},
		"git blame -L 1,1 util.go":                   {"package main"},
		"git shortlog -sn HEAD":                      {"2\tn"},
		"git config --get user.name":                 {"[exit "}, // set or not, the point is that it runs
		"git ls-files | sort -r | head -n 1":         {"util.go\n"},
		"git ls-files | cut -d . -f 2 | sort | uniq": {"go\nmd\n"},
		"ls >/dev/null && echo-no":                   {}, // not auto: echo-no is not a command
		"pwd && pwd":                                 {"[exit 0]"},
		"git log -S hello --oneline":                 {"first"},
		"git log -n 1 --format='%h %s'":              {" second"},
		"git log --author n -n 1 --oneline":          {"second"},
	} {
		c := an.Check(context.Background(), cmd)
		if len(want) == 0 {
			if c.Auto {
				t.Errorf("%q should not be auto-allowed", cmd)
			}
			continue
		}
		if !c.Auto {
			t.Errorf("%q is not auto-allowed: %+v", cmd, c.Stages)
			continue
		}
		out, err := call(context.Background(), b, stamped(t, dir, cmd))
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range want {
			if !strings.Contains(out, w) {
				t.Errorf("%q: output lacks %q:\n%s", cmd, w, out)
			}
		}
	}
}

func TestRenderQuotesEveryWordButGlobs(t *testing.T) {
	an := &Analyzer{Files: newWS(t, t.TempDir())}
	for cmd, want := range map[string]string{
		"ls -la *.go sub":                   "'ls' '-la' 'sub' '--' *.go",
		"ls -d */ x? -l":                    "'ls' '-d' '-l' '--' */ x?",
		"ls -- *.go":                        "'ls' '--' *.go",
		"ls -l -- -n *.go":                  "'ls' '-l' '--' '-n' *.go",
		"ls -la sub":                        "'ls' '-la' 'sub'",
		"git log --format='%h %s' -n 1":     "'git' 'log' '--no-ext-diff' '--no-textconv' '--format=%h %s' '-n' '1'",
		`git log --grep="it's" -n 1`:        `'git' 'log' '--no-ext-diff' '--no-textconv' '--grep=it'\''s' '-n' '1'`,
		"git status 2>&1 | head -n 3":       "'git' 'status' 2>&1 | 'head' '-n' '3'",
		"cd . && ls *.go":                   "'cd' '.' && 'ls' '--' *.go",
		"git status && git diff >/dev/null": "'git' 'status' && 'git' 'diff' '--no-ext-diff' '--no-textconv' >/dev/null",
	} {
		c := an.Check(context.Background(), cmd)
		if !c.Parsed {
			t.Errorf("%q does not parse", cmd)
			continue
		}
		if got := c.Render(); got != want {
			t.Errorf("%q renders as\n%s\nwant\n%s", cmd, got, want)
		}
	}
}

func TestParsePlanShapes(t *testing.T) {
	for cmd, n := range map[string][2]int{ // pipelines, stages in total
		"a":                     {1, 1},
		"a && b":                {2, 2},
		"a | b | c":             {1, 3},
		"a | b && c | d":        {2, 4},
		"a 2>&1":                {1, 1},
		"a >/dev/null 2>&1 | b": {1, 2},
		"a  &&  b":              {2, 2},
	} {
		p, ok := parsePlan(cmd)
		if !ok {
			t.Errorf("%q does not parse", cmd)
			continue
		}
		stages := 0
		for _, pl := range p.pipelines {
			stages += len(pl)
		}
		if len(p.pipelines) != n[0] || stages != n[1] {
			t.Errorf("%q: %d pipelines, %d stages; want %v", cmd, len(p.pipelines), stages, n)
		}
	}
	for _, cmd := range []string{"", "&&", "a &&", "&& a", "a | ", "| a", "a || b", "a & b", "a &", "a ; b", "a > b", "a >/dev/null/x", ">/dev/null", "2>&1", "a 2>&1x", "FOO=1 a", "*.go", "a $(b)", "a `b`"} {
		if _, ok := parsePlan(cmd); ok {
			t.Errorf("%q should not parse", cmd)
		}
	}
}

// R3-6 of the third review: an ls glob that expanded to -n or
// --output=x passed it to ls as an option.
func TestAnLsGlobExpandsAfterDoubleDash(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"-n", "--output=x", "-la", "a.go", "b.go"} {
		os.WriteFile(filepath.Join(dir, n), []byte("x\n"), 0o644)
	}
	b := Bash(newWS(t, dir))
	for _, cmd := range []string{"ls *", "ls -1 *", "ls -d *", "ls -l -- *", "ls *.go -l", "ls -- *"} {
		c := (&Analyzer{Files: newWS(t, dir)}).Check(context.Background(), cmd)
		if !c.Auto {
			t.Errorf("%q is not auto-allowed: %+v", cmd, c.Stages)
			continue
		}
		out, err := call(context.Background(), b, stamped(t, dir, cmd))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "unrecognized") || strings.Contains(out, "invalid option") || !strings.Contains(out, "[exit 0]") {
			t.Errorf("%q: %q", cmd, out)
		}
	}
	out, _ := call(context.Background(), b, stamped(t, dir, "ls -1 *"))
	if !strings.Contains(out, "--output=x\n") || !strings.Contains(out, "-n\n") || !strings.Contains(out, "-la\n") {
		t.Errorf("the odd names are not listed as names: %q", out)
	}
}
