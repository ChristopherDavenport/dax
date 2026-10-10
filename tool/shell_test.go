package tool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	workspace "github.com/ChristopherDavenport/agentworkspace"
)

// wd is a real directory: deciding a git command reads its config.
var wd = func() string {
	d, err := os.MkdirTemp("", "dax-tool-")
	if err != nil {
		panic(err)
	}
	return d
}()

func subjects(t *testing.T, cmd string) ([]subj, error) {
	t.Helper()
	args, _ := json.Marshal(map[string]string{"command": cmd})
	got, err := BashSubjects(newWS(t, wd), 0)(t.Context(), args)
	var have []subj
	for _, s := range got {
		var m map[string]string
		json.Unmarshal(s.Args, &m)
		field := "command"
		if s.Tool == "write" {
			field = "path"
		}
		_ = field
		have = append(have, subj{s.Tool, m[field], s.Text})
	}
	return have, err
}

type subj struct{ tool, match, text string }

func TestBashSubjects(t *testing.T) {
	const s = sentinel
	tests := []struct {
		cmd     string
		want    []subj
		wantErr string
	}{
		// Inside the safe subset: one subject, words normalised.
		{"git status", []subj{{"", "git status", "git status"}}, ""},
		{"  git   status   -s ", []subj{{"", "git status -s", "git status -s"}}, ""},
		{`ls "a b"`, []subj{{"", "ls a b", "ls a b"}}, ""},
		// Governed but not read-only: the command, then a sentinel.
		{"git log --output=/x", []subj{{"", "git log --output=/x", "git log --output=/x"}, {"", s + "git log --output=/x", "git log --output=/x  [the flag --output=/x is not known to be read-only]"}}, ""},
		// Outside the subset: the parts a rule can name, then a sentinel.
		// A line in the subset is one subject per stage; rm is not a
		// command the check governs, so the rules decide it.
		{"git status && rm -rf /", []subj{{"", "git status", "git status"}, {"", "rm -rf /", "rm -rf /"}}, ""},
		{"git status && git log --output=/x", []subj{{"", "git status", "git status"}, {"", "git log --output=/x", "git log --output=/x"}, {"", s + "git log --output=/x", "git log --output=/x  [the flag --output=/x is not known to be read-only]"}}, ""},
		{"git log | head -n 5 2>&1", []subj{{"", "git log", "git log"}, {"", "head -n 5", "head -n 5"}}, ""},
		// Outside the subset: the parts, then a sentinel.
		{"git status; rm -rf /", []subj{{"", "git status", "git status"}, {"", "rm -rf /", "rm -rf /"}, {"", s + "git status; rm -rf /", "git status; rm -rf /"}}, ""},
		{"a; b || c | d & e\nf", []subj{{"", "a", "a"}, {"", "b", "b"}, {"", "c", "c"}, {"", "d", "d"}, {"", "e", "e"}, {"", "f", "f"}, {"", s + "a; b || c | d & e\nf", "a; b || c | d & e\nf"}}, ""},
		{"git log > out.txt", []subj{{"", "git log", "git log"}, {"write", "out.txt", "out.txt"}, {"", s + "git log > out.txt", "git log > out.txt"}}, ""},
		{"go test 2>&1", []subj{{"", "go test", "go test"}}, ""},
		{"echo 'unterminated", nil, "unterminated quote"},
		{"   ", nil, "empty"},
	}
	for _, tc := range tests {
		t.Run(tc.cmd, func(t *testing.T) {
			have, err := subjects(t, tc.cmd)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(have, tc.want) {
				t.Fatalf("got  %q\nwant %q", have, tc.want)
			}
		})
	}
}

// What the user is asked about has to be what bash will run, so the
// parts of an asked command are read the way bash reads them.
func TestTheSubjectsOfAnAskedCommandAreWhatBashRuns(t *testing.T) {
	const s = sentinel
	tests := []struct {
		name string
		cmd  string
		want []string // the subjects before the sentinel
	}{
		// #1 of the review: quotes inside comments do not open a string.
		{"comment hides quotes (single)", "git status #'\n touch /x\n#'", []string{"git status", "touch /x"}},
		{"comment hides quotes (double)", "git status #\"\n touch /x\n#\"", []string{"git status", "touch /x"}},
		{"$'\\'' is a quote character, the ; is live", `git status $'\'' ; touch /x #'`, []string{"git status $'\\''", "touch /x"}},
		{"$'\\'' with a redirect", `git status -- $'\'' >/x #'`, []string{"git status -- $'\\''", "write:/x"}},
		{"a comment is not a command", "git status # rm -rf /", []string{"git status"}},
		{"# inside a word is not a comment", "git log a#b", []string{"git log a#b"}},
		// #2: >&word is a redirect to a file.
		{">&path", "git status >&/home/u/.bashrc", []string{"git status", "write:/home/u/.bashrc"}},
		{">& path", "git status >& /home/u/.bashrc", []string{"git status", "write:/home/u/.bashrc"}},
		{">&$HOME/x", "git status >&$HOME/x", []string{"git status", "write:$HOME/x"}},
		{">&2foo is a file named 2foo", "git status >&2foo", []string{"git status", "write:2foo"}},
		{">&2 is a descriptor", "git status >&2", []string{"git status >&2"}},
		{">&- closes", "git status >&-", []string{"git status >&-"}},
		{"&>> appends", "git status &>>/x", []string{"git status", "write:/x"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			have, err := subjects(t, tc.cmd)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, h := range have[:len(have)-1] {
				if h.tool == "write" {
					got = append(got, "write:"+h.match)
				} else {
					got = append(got, h.match)
				}
			}
			if last := have[len(have)-1]; !strings.HasPrefix(last.match, s) {
				t.Errorf("no sentinel last: %q", last)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestSafeWords(t *testing.T) {
	ok := map[string][]string{
		"git status":                       {"git", "status"},
		"git log --oneline -n5":            {"git", "log", "--oneline", "-n5"},
		"git diff HEAD~1 HEAD^":            {"git", "diff", "HEAD~1", "HEAD^"},
		`git log --grep='a #1 b'`:          {"git", "log", "--grep=a #1 b"},
		`git log --format="%h %s"`:         {"git", "log", "--format=%h %s"},
		"git show main:cmd/dax/main.go":    {"git", "show", "main:cmd/dax/main.go"},
		"ls\t-la  ./internal":              {"ls", "-la", "./internal"},
		`ls "my dir"'/x'`:                  {"ls", "my dir/x"},
		"git diff --stat=80,40 a..b c...d": {"git", "diff", "--stat=80,40", "a..b", "c...d"},
	}
	for cmd, want := range ok {
		got, isSafe := safeWords(cmd)
		if !isSafe || !reflect.DeepEqual(got, want) {
			t.Errorf("safeWords(%q) = %q, %v; want %q", cmd, got, isSafe, want)
		}
	}
	for _, cmd := range []string{
		"", "   ", "a; b", "a && b", "a || b", "a | b", "a & b", "a\nb", "a #c", "a # c", "a$HOME", "a ${x}", "a $(x)", "a `x`",
		"a > b", "a >b", "a < b", "a >&2", "a 2>&1", "a\\ b", "a *.go", "a ?", "a [x]", "a {b,c}", "a ~", "a ~/x", "~ a",
		"X=1 git status", "git=1 status", "a $'x'", `a $"x"`, `a "$x"`, `a "x\y"`, "a \"`x`\"", "a 'unterminated", `a "unterminated`,
		"a \x00", "a é", "a;b", "a b", "a \"x\ny\"", "a !x", "a (b)", "git status\r",
	} {
		if got, isSafe := safeWords(cmd); isSafe {
			t.Errorf("safeWords(%q) = %q, want not safe", cmd, got)
		}
	}
}

func TestReadOnlyArgs(t *testing.T) {
	good := []string{
		"git status", "git status -s -b", "git status --porcelain=v2", "git diff", "git diff --cached --stat", "git diff HEAD~1 -- internal/tool",
		"git diff --no-ext-diff --name-only", "git log --oneline -n5 -5", "git log --since=2.days --author=me --grep=fix",
		"git log --format=%h -- cmd", "git show HEAD", "git show --stat HEAD~2", "git diff main..HEAD", "git log -U3 -p",
		"ls", "ls -la", "ls --color=always", "ls -l ./internal", "ls --all .", "ls -- x", "pwd", "go version", "go env GOPATH GOFLAGS", "go env -json GOROOT",
		"make check", "git commit -m x", "git",
	}
	bad := []string{
		"git log --output=/x", "git diff --output /x", "git show --output=x", "git diff -o x", "git log -o x",
		"git diff --ext-diff", "git diff --textconv", "git log --ext-diff", "git -c core.pager=x log", "git -c x=y status",
		"git -C /other status", "git -C . status", "git --git-dir=/x status", "git --work-tree=/x status", "git --exec-path=/x status",
		"git diff --no-index /dev/null /etc/passwd", "git diff --no-index a b", "git log --exec-path", "git status --git-dir=x",
		"git diff -C", "git log --unknown", "git status -x", "git diff /etc/passwd", "git log -- /etc", "git show ../x", "git diff -- ../x",
		"git log -- '~/x'", "git diff HEAD:../../x ../..",
		"ls /etc", "ls ..", "ls ../x", "ls -la /", "ls -I x", "ls -z", "ls ./../..",
		"pwd -P", "pwd x", "go env", "go env -json", "go version -m x", "go env -w GOFLAGS=-x", "go env -u GOFLAGS", "go env GOFLAGS=-x", "go env -changed",
	}
	for _, cmd := range good {
		w, ok := safeWords(cmd)
		if !ok {
			t.Fatalf("%q is not in the safe subset", cmd)
		}
		if !readOnlyArgs(w, newWS(t, wd)) {
			t.Errorf("readOnlyArgs(%q) = false, want true", cmd)
		}
	}
	for _, cmd := range bad {
		w, ok := safeWords(cmd)
		if !ok {
			t.Fatalf("%q is not in the safe subset", cmd)
		}
		if readOnlyArgs(w, newWS(t, wd)) {
			t.Errorf("readOnlyArgs(%q) = true, want false", cmd)
		}
	}
}

// #47: a redirect's subjects are the files it writes, as a file tool's
// are: the name normalised from where the line is (after a plain cd,
// and from the root as well), and what the links on its way lead to,
// read through the workspace. A link out adds a write no rule names, a
// workspace that cannot read links a subject no rule names; what bash
// expands, /dev/null and a target outside the workspace are as before.
func TestARedirectsSubjectsAreTheFilesItWrites(t *testing.T) {
	const s = sentinel
	dir, outside := t.TempDir(), t.TempDir()
	for name, content := range map[string]string{".env": "SECRET=1\n", "README.md": "# hi\n", "sub/a.txt": "a\n"} {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
	}
	for link, target := range map[string]string{"notes": ".env", "docs": "sub", "out": outside, "sub/up": ".."} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	local, err := workspace.NewLocal(dir, DefaultEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { local.Close() })
	tests := []struct {
		cmd     string
		writes  []string // tool:path of every subject that is not a command
		noLinks []string // the same in a workspace that cannot read links
	}{
		{"echo x > README.md", []string{"write:README.md"}, []string{"write:README.md", unresolvedTool + ":README.md"}},
		{"echo x > notes", []string{"write:notes", "write:.env"}, []string{"write:notes", unresolvedTool + ":notes"}},
		{"echo x >> ./sub/../notes", []string{"write:notes", "write:.env"}, nil},
		{"echo x > " + filepath.Join(dir, "notes"), []string{"write:notes", "write:.env"}, nil},
		{"echo x > docs/new.txt", []string{"write:docs/new.txt", "write:sub/new.txt"}, nil},
		{"echo x > out/x", []string{"write:out/x", "write:" + s + "out/x"}, nil},
		// After a cd: from the root, as before, and from where the cd leads.
		{"cd sub && echo x > ../notes", []string{"write:../notes", "write:notes", "write:.env"}, nil},
		{"cd docs && echo x > a.txt", []string{"write:a.txt", "write:docs/a.txt", "write:sub/a.txt"}, nil},
		{"cd docs && echo x > up/notes", []string{"write:up/notes", "write:docs/up/notes", "write:.env"}, nil},
		{"(cd sub; echo x > ../notes)", []string{"write:../notes", "write:notes", "write:.env"}, nil},
		{"cd $D && echo x > ../notes", []string{"write:../notes"}, nil},
		// A .. is where the names before it really are: sub/up is the root.
		{"echo x > sub/up/notes", []string{"write:sub/up/notes", "write:.env"}, nil},
		// Unchanged: /dev/null, a descriptor, what bash expands, outside.
		{"echo x > /dev/null; true", nil, nil},
		{"echo x >&2; true", nil, nil},
		{"echo x > $HOME/x", []string{"write:$HOME/x"}, nil},
		{"echo x > ~/.bashrc", []string{"write:~/.bashrc"}, nil},
		{"echo x > /etc/passwd", []string{"write:/etc/passwd"}, nil},
		{"echo x > ../x", []string{"write:../x"}, nil},
	}
	for _, ws := range []struct {
		name string
		ws   workspace.Workspace
	}{{"local", local}, {"no links", noLinks{local}}} {
		f := NewFiles(ws.ws)
		for _, tc := range tests {
			want := tc.writes
			if ws.name == "no links" {
				if tc.noLinks == nil {
					continue
				}
				want = tc.noLinks
			}
			t.Run(ws.name+"/"+tc.cmd, func(t *testing.T) {
				args, _ := json.Marshal(map[string]string{"command": tc.cmd})
				got, err := BashSubjects(f, 0)(t.Context(), args)
				if err != nil {
					t.Fatal(err)
				}
				var writes []string
				for _, sj := range got {
					if sj.Tool == "" {
						continue
					}
					var m map[string]string
					json.Unmarshal(sj.Args, &m)
					writes = append(writes, sj.Tool+":"+m["path"])
				}
				if !reflect.DeepEqual(writes, want) {
					t.Fatalf("got  %q\nwant %q", writes, want)
				}
				if last := got[len(got)-1]; !strings.HasPrefix(string(last.Args), `{"command":"`+s) {
					t.Errorf("no sentinel last: %s", last.Args)
				}
			})
		}
	}
}
