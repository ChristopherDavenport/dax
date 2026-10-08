package tool

import (
	"context"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The differential test is the property that makes the analyzer worth
// trusting: for every line it auto-allows, what real bash does with the
// rendered line is what the analyzer parsed. Each stage's command is
// replaced by a probe that records its argv; bash runs the line in a
// directory of glob-matchable files; and the recorded argv of every
// stage must be the parsed words, a glob word standing for the files
// the analyzer checked, and nothing else may have run (a command
// smuggled in through a word shows as a file a probe never wrote).

const probeScript = `#!/bin/sh
idx=$1; shift
: > "$PROBE_LOG/$idx"
for a in "$@"; do printf '%s\0' "$a" >> "$PROBE_LOG/$idx"; done
`

type diffEnv struct {
	t     *testing.T
	dir   string
	probe string
	log   string
	an    *Analyzer
}

func newDiffEnv(t *testing.T) *diffEnv {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	for _, name := range []string{
		"a.go", "b.go", "a b.go", "-n", "--output=x", "x;y", "it's.go", "q\"d.go", "$HOME", "sub/c.go", "sub/d.txt", "sub/deep/e.go", "sub2/f.go", ".hidden", "sub/.dot", "star*name", "é.go",
	} {
		p := filepath.Join(work, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	probe := filepath.Join(dir, "probe")
	if err := os.WriteFile(probe, []byte(probeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "log")
	os.Mkdir(log, 0o755)
	return &diffEnv{t: t, dir: work, probe: probe, log: log, an: &Analyzer{Dir: work, ConfigKey: func(context.Context, string, bool) (string, error) { return "", nil }}}
}

// run checks one line, and reports whether it was auto-allowed and so
// exercised.
func (e *diffEnv) run(cmd string) (exercised bool) {
	t := e.t
	t.Helper()
	c := e.an.Check(context.Background(), cmd)
	if !c.Auto {
		return false
	}
	// Build the probing line: every stage but cd is its probe with its
	// stage number first; cd runs, so the globs after it expand where
	// the analyzer thought they would.
	p := *c.plan
	var want [][]string // per stage, the words after the command
	var emittedStages [][]word
	var wantCwd []string
	cwd := e.dir
	idx := 0
	probing := &plan{}
	for _, pl := range p.pipelines {
		var stages []stage
		for _, st := range pl {
			if len(pl) == 1 && st.words[0].text == "cd" {
				stages = append(stages, st)
				cwd = filepath.Join(cwd, st.words[1].text)
				continue
			}
			// What bash is given is the stage's emitted words: its own with
			// the flags and -- the analyzer adds. The probe stands for the
			// command.
			em := st.emitted()
			ns := stage{redirects: st.redirects}
			ns.words = append(ns.words, word{text: e.probe}, word{text: strconv.Itoa(idx)})
			ns.words = append(ns.words, em[1:]...)
			stages = append(stages, ns)
			emittedStages = append(emittedStages, em)
			want = append(want, stage{words: em}.texts()[1:])
			wantCwd = append(wantCwd, cwd)
			idx++
		}
		probing.pipelines = append(probing.pipelines, stages)
	}
	line := probing.render()
	os.RemoveAll(e.log)
	os.Mkdir(e.log, 0o755)
	cmdr := exec.Command("bash", "-c", line)
	cmdr.Dir = e.dir
	cmdr.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C", "PROBE_LOG=" + e.log, "HOME=" + e.dir}
	out, _ := cmdr.CombinedOutput()

	// Nothing but the probes ran: the working directory holds only the
	// files it started with.
	entries, _ := os.ReadDir(e.dir)
	for _, en := range entries {
		switch en.Name() {
		case "a.go", "b.go", "a b.go", "-n", "--output=x", "x;y", "it's.go", "q\"d.go", "$HOME", "sub", "sub2", ".hidden", "star*name", "é.go":
		default:
			t.Fatalf("%q\nrendered as\n%s\nran something else: %q appeared (output %q)", cmd, line, en.Name(), out)
		}
	}
	for i, w := range want {
		raw, err := os.ReadFile(filepath.Join(e.log, strconv.Itoa(i)))
		if err != nil {
			t.Fatalf("%q\nrendered as\n%s\nstage %d never ran (output %q)", cmd, line, i, out)
		}
		var got []string
		if len(raw) > 0 {
			got = strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
		}
		if err := e.compare(emittedStages[i], wantCwd[i], w, got); err != "" {
			t.Fatalf("%q\nrendered as\n%s\nstage %d: %s\n  bash ran it with %q\n  the analyzer parsed %q", cmd, line, i, err, got, w)
		}
	}
	return true
}

// compare checks a stage's argv as bash expanded it against the words
// the analyzer parsed. A non-glob word is itself. A glob word is some
// of the files the analyzer's own glob finds (bash leaves out dotfiles
// and, in the C locale, multibyte matches for ?, so the analyzer's set
// is the larger one), in order, or the pattern itself when bash found
// none; a directory-only pattern (ending /) is matched by directories.
func (e *diffEnv) compare(em []word, cwd string, words, got []string) string {
	var globs []bool
	for _, w := range em[1:] {
		globs = append(globs, w.glob)
	}
	pos := 0
	for i, w := range words {
		if !globs[i] {
			if pos >= len(got) || got[pos] != w {
				return "word " + strconv.Quote(w) + " is not what bash passed"
			}
			pos++
			continue
		}
		pat := strings.TrimSuffix(w, "/")
		dirOnly := pat != w
		matches, _ := globFS(os.DirFS(cwd), pat)
		allowed := map[string]bool{}
		for _, m := range matches {
			fi, err := os.Stat(filepath.Join(cwd, m))
			if dirOnly && (err != nil || !fi.IsDir()) {
				continue
			}
			if dirOnly {
				m += "/"
			}
			allowed[m] = true
		}
		if pos < len(got) && got[pos] == w {
			pos++ // bash found nothing and passed the pattern
			continue
		}
		var prev string
		for pos < len(got) && allowed[got[pos]] && got[pos] > prev {
			prev = got[pos]
			pos++
		}
	}
	if pos != len(got) {
		return "bash passed words the analyzer did not parse, from " + strconv.Quote(got[pos])
	}
	return ""
}

// The corpus: every line the policy tests say must run unasked, plus
// the reviewer's R3-1 strings, which must not.
func TestDifferentialCorpus(t *testing.T) {
	e := newDiffEnv(t)
	corpus := []string{
		"git status", "git log -n 5 --oneline", "git status 2>&1", "git log --oneline | head -n 5", "git log | wc -l", "ls", "ls -la", "ls *.go", "ls -d */",
		"ls sub/*.go", "ls ?.go", "ls sub/*", "ls */", "ls -d s*", "cat a.go", "cat 'a b.go'", "head -n 1 a.go b.go", "wc -l a.go", "grep x a.go b.go", "cd sub && ls", "cd sub && ls *.go",
		"cd sub/deep && ls *.go", "ls && pwd", "pwd", "git ls-files | sort | uniq -c", "ls *.go | head -n 2", "ls 2>/dev/null", "ls >/dev/null 2>&1", "cat 'it'\"'\"'s.go'", `cat "q\"d.go"`,
		"cat -- -n", "ls -- -n", "cat 'x;y'", "cat '$HOME'", "cat 'star*name'", "ls star*", "ls 'star*'", "ls *name", "ls é.go", "cat é.go", "ls '*'", "ls \"?\"",
	}
	exercised := 0
	for _, cmd := range corpus {
		if e.run(cmd) {
			exercised++
		}
	}
	if exercised < 25 {
		t.Errorf("only %d corpus lines were auto-allowed; the generator is not testing what it should", exercised)
	}
	// R3-1 of the third review: a quoted part in a glob word was
	// emitted bare. These must not be auto-allowed at all.
	for _, cmd := range []string{
		"ls ';touch PWN;'*", "ls '$(touch PWN)'*", "ls '`touch PWN`'?", `ls "a&touch PWN&"*`, "ls '>PWN'*", `ls "$(touch PWN)"*`,
		"ls *';touch PWN;'", "ls a*';touch PWN;'b", "ls '*'*", "ls *'a'", "ls '/'*", "ls ';'*",
	} {
		if c := e.an.Check(context.Background(), cmd); c.Auto {
			t.Errorf("%q is auto-allowed; rendered as %s", cmd, c.Render())
		}
	}
}

// Fragments the generator builds hostile words from.
var fragments = []string{
	"'", `"`, "$", "`", ";", "&", "|", "<", ">", " ", "\n", "*", "?", "[", "]", "-", `\`, "é", "a", "b", ".go", "/", "(", ")", "{", "}", "#", "!", "~", "^", "=", ":", "%", ",", "+", "@",
	"$(touch PWN)", "`touch PWN`", ";touch PWN;", "&touch PWN&", ">PWN", "'a b'", `"a b"`, "'x;y'", "sub", "../", "..", ".", "*.go", "?.go", "sub/*", "$HOME", "${x}", "\t", "2>&1", ">/dev/null",
	"*'", "'*", `*"`, `"*`, "a*", "*b", "*'a'", "'a'*", "';touch PWN;'*", "'$(touch PWN)'?",
}

var commands = []string{
	"ls", "ls -d", "ls -la", "cat", "head -n 1", "wc -l", "grep x", "pwd", "git status", "git log --oneline -n 1", "git diff --stat", "cd", "ls --", "tail -n 1",
}

func hostileWord(r *rand.Rand) string {
	var b strings.Builder
	for i, n := 0, 1+r.Intn(4); i < n; i++ {
		b.WriteString(fragments[r.Intn(len(fragments))])
	}
	return b.String()
}

func hostileLine(r *rand.Rand) string {
	var b strings.Builder
	for i, n := 0, 1+r.Intn(3); i < n; i++ {
		if i > 0 {
			b.WriteString([]string{" && ", " | ", " && ", " | ", " ; "}[r.Intn(5)])
		}
		b.WriteString(commands[r.Intn(len(commands))])
		for j, m := 0, r.Intn(4); j < m; j++ {
			b.WriteString(" ")
			if r.Intn(3) == 0 {
				b.WriteString([]string{"a.go", "*.go", "sub", "sub/*", "-n", "a", "'a b.go'"}[r.Intn(7)])
			} else {
				b.WriteString(hostileWord(r))
			}
		}
		if r.Intn(6) == 0 {
			b.WriteString(" " + []string{"2>&1", ">/dev/null", "2>/dev/null"}[r.Intn(3)])
		}
	}
	return b.String()
}

// TestDifferentialFuzz generates lines from a hostile alphabet and runs
// each one the analyzer auto-allows through real bash. DAX_FUZZ_LINES
// sets how many lines (default 3000).
func TestDifferentialFuzz(t *testing.T) {
	e := newDiffEnv(t)
	lines := 3000
	if s := os.Getenv("DAX_FUZZ_LINES"); s != "" {
		lines, _ = strconv.Atoi(s)
	}
	exercised := 0
	for seed := 0; seed < lines; seed++ {
		r := rand.New(rand.NewSource(int64(seed)))
		if e.run(hostileLine(r)) {
			exercised++
		}
	}
	t.Logf("%d of %d generated lines were auto-allowed and checked against bash", exercised, lines)
	if exercised < lines/50 {
		t.Errorf("only %d of %d lines were auto-allowed; the generator is too hostile to test the auto path", exercised, lines)
	}
}
