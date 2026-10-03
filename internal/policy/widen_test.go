package policy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ChristopherDavenport/agentturn"
)

// workspace builds a small project with a git repository.
func workspace(t *testing.T) string {
	t.Helper()
	dir := hostileRepo(t, "")
	for name, content := range map[string]string{
		"main.go": "package main\n", "util.go": "package main\n", "README.md": "# hi\n", "sub/a.go": "package sub\n", "sub/deep/b.go": "package deep\n",
	} {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
	}
	return dir
}

func table(t *testing.T, dir string, want agentturn.ToolAction, cmds ...string) {
	t.Helper()
	for _, cmd := range cmds {
		if got, reason := decideIn(t, dir, defaults, "bash", bash(cmd)); got != want {
			t.Errorf("%q = %v (%s), want %v", cmd, got, reason, want)
		}
	}
}

// Everything the second review found asking that people do all day.
func TestTheUsualReadOnlyCommandsRunWithoutAsking(t *testing.T) {
	dir := workspace(t)
	table(t, dir, agentturn.Allow,
		// (a) combined short flags and space-separated values
		"git status -sb", "git status -sbv", "git log -n 5", "git log -n 5 --oneline", "git log --author chris", "git log --author=chris",
		"git log -S foo", "git log -G foo", "git log -Sfoo", "git log -n5", "git log --since 2.weeks", "git log --grep fix -i",
		"git diff --color-moved", "git diff --color-moved=zebra", "git diff -U 5", "git diff -pw", "git log -ps", "git show -s HEAD",
		"git log --max-count 3", "git diff --stat -M", "git log -m -p -n 1",
		// (b) trailing redirects
		"git status 2>&1", "git diff 2>/dev/null", "git log -n 3 >/dev/null", "git status >/dev/null 2>&1", "ls 2>/dev/null", "git log 2>&1 >/dev/null",
		// (c) pipelines
		"git log --oneline | head", "git log --oneline | head -n 5", "git log | head -5", "git log --oneline | wc -l", "git diff | tail -n 20",
		"git log --oneline | grep fix", "git log --oneline | grep -i fix | head -n 3", "git ls-files | sort | uniq -c", "git log --format=%an | sort | uniq -c | sort -rn | head -n 5",
		"ls | cut -d . -f 1", "git log --oneline 2>&1 | head", "ls -la | grep -v total", "git branch | wc -l", "git log | tail -n +3",
		// (d) read-only subcommands
		"git branch", "git branch -a", "git branch -r", "git branch -vv", "git branch --list", "git branch --list 'feat*'", "git branch --show-current",
		"git branch --merged main", "git branch --contains HEAD", "git rev-parse HEAD", "git rev-parse --short HEAD", "git rev-parse --abbrev-ref HEAD",
		"git rev-parse --show-toplevel", "git ls-files", "git ls-files -m", "git ls-files -o --exclude-standard", "git ls-files sub", "git remote", "git remote -v",
		"git blame main.go", "git blame -L 1,5 main.go", "git blame -w -M main.go", "git stash list", "git stash list --oneline", "git tag", "git tag -l", "git tag --list 'v1*'",
		"git tag --sort=-creatordate", "git tag --contains HEAD", "git describe", "git describe --tags --always", "git shortlog -sn", "git shortlog -sn HEAD",
		"git config --get user.name", "git config --get-all remote.origin.fetch", "git config --list", "git config --local --get core.bare", "git config -l --show-origin",
		// (e) reading files
		"cat main.go", "cat -n main.go util.go", "head main.go", "head -n 5 main.go", "head -5 main.go", "head -c 100 main.go", "tail -n 3 README.md", "wc -l main.go util.go",
		"wc main.go", "grep package main.go", "grep -n package main.go util.go", "grep -i hello README.md", "grep -c package sub/a.go", "grep -e package main.go",
		"cat sub/a.go", "cat ./main.go", "cat missing.txt", "head --lines=5 main.go",
		// (f) ls with globs
		"ls *.go", "ls -d */", "ls -la *.md", "ls sub/*.go", "ls sub/*", "ls */", "ls -d sub/*/", "ls ?ain.go", "ls --color=auto", "ls --time-style=long-iso -l",
		"ls *.nothing", "ls -d s*",
		// (g) cd and sequences
		"cd sub && ls", "cd sub && git status", "cd sub && cat a.go", "cd sub/deep && ls -la", "cd sub && cd deep && ls", "git status && git diff", "git status && git log -n 3",
		"cd sub && git log --oneline | head -n 3", "cd . && ls", "git branch && git status -sb", "ls && pwd",
	)
}

// Each widening keeps every earlier exploit asking.
func TestTheWideningsStayInsideTheSubset(t *testing.T) {
	dir := workspace(t)
	os.Symlink("/etc", filepath.Join(dir, "etclink"))
	os.MkdirAll(filepath.Join(dir, "d"), 0o755)
	os.Symlink(filepath.Join(dir, "d"), filepath.Join(dir, "inlink"))
	big := filepath.Join(dir, "big.log")
	os.WriteFile(big, make([]byte, 3<<20), 0o644)
	table(t, dir, agentturn.Defer,
		// (a)
		"git log -n", "git log --author", "git log --output x", "git log -o x", "git diff --output=/x", "git log -c", "git log -C", "git status -sbo",
		"git log -S", "git diff -sbX", "git log --author x --output=/x", "git log -n 5 -- /etc", "git log -Ox", "git log -sbc", "git log --pretty %GG",
		"git log --format %GS", "git log -n 5 --ext-diff", "git diff --color-moved --textconv",
		// (b) redirects that write or read, in every spelling
		"git status > /tmp/x", "git status >/tmp/x", "git status 2>/tmp/x", "git status >>/dev/null", "git status &>/dev/null", "git status 2>&1 >/tmp/x",
		"git status >&2", "git status 1>&2", "git status > /dev/null", "git status 2> /dev/null", "git status >/dev/nullx", "git status 2>&1x", "git status<x",
		"git status >/dev/null/../x", "git status 2>/dev/null; touch x", "cat main.go >/dev/null >x", ">/dev/null git status", "git status 3>/dev/null",
		"git status 2>&1&", "git status |& cat", "git status >|/dev/null",
		// (c) pipelines
		"git log | sh", "git log | bash", "git log | cat", "git log | tee x", "git log | xargs rm", "git log | head main.go", "git log | tail -f", "git log | grep -r fix",
		"git log | grep -f x", "git log | grep fix main.go", "git log | sort -o x", "git log | sort -T /tmp", "git log | sort --compress-program=sh", "git log | uniq x y",
		"git log | cut -f1 main.go", "git log | wc -l main.go", "git log | head -n x", "git log | grep", "git log || cat", "git log |", "| git log", "git log | | head",
		"git log --output=/x | head", "git log | head; rm x", "ls | git status", "cat main.go | sort -o main.go", "git log | awk 1", "git log | sed s/a/b/", "git log | sort -k 1;x",
		"git log | head -c 99999999999999999999x", "git log | dd of=x", "git log | python", "git log | less", "git log | head `x`", "git log | tail -n $(x)",
		// (d)
		"git branch -D x", "git branch -d x", "git branch newbranch", "git branch -m a b", "git branch -c a b", "git branch --set-upstream-to=x", "git branch -u x",
		"git branch --edit-description", "git branch -f x", "git branch -v foo", "git branch -a foo", "git branch --list --delete x", "git branch --unset-upstream",
		"git rev-parse --git-path hooks", "git rev-parse --parseopt", "git rev-parse --sq-quote", "git rev-parse HEAD:secret", "git rev-parse --local-env-vars --x",
		"git ls-files -X /etc/passwd", "git ls-files --exclude-from=/etc/passwd", "git ls-files --recurse-submodules", "git ls-files /etc", "git ls-files ../x",
		"git remote add x y", "git remote remove origin", "git remote set-url origin x", "git remote update", "git remote show origin", "git remote -v update", "git remote prune origin",
		"git blame --contents /etc/passwd main.go", "git blame -S /etc/passwd main.go", "git blame /etc/passwd", "git blame ../x", "git blame -C main.go",
		"git stash", "git stash pop", "git stash drop", "git stash apply", "git stash push", "git stash list -p --output=x", "git stash list main.go", "git stash show -p",
		"git tag v1", "git tag -a v1 -m x", "git tag -d v1", "git tag -f v1", "git tag -s v1", "git tag v1 HEAD", "git tag --delete v1", "git tag -m x v1",
		"git describe --dirty x:y", "git describe HEAD:x", "git shortlog -sn --output=x", "git shortlog /etc",
		"git config user.name x", "git config --global user.name x", "git config --add a.b c", "git config --unset a.b", "git config -e", "git config --edit",
		"git config --get", "git config --get a.b c d", "git config --list a.b", "git config --file /etc/x --list", "git config -f x --get a.b", "git config --get a.b=c",
		"git config --replace-all a.b c", "git config --remove-section a", "git config --rename-section a b", "git config --get --list", "git config --blob HEAD:x --list",
		"git config --get core.sshCommand=x", "git config", "git config --get core.x:y",
		// (e) files outside, links, big files, devices, flags
		"cat /etc/passwd", "cat ../x", "cat ~/.ssh/id_rsa", "cat etclink/passwd", "cat inlink/../x", "head /dev/zero", "cat /dev/stdin", "cat -", "tail -f main.go", "tail -F main.go",
		"cat big.log", "head big.log", "wc big.log", "grep x big.log", "grep -r package .", "grep -R package .", "grep package .", "grep package sub", "grep -f main.go util.go",
		"grep --include=*.go x main.go", "grep -r x /etc", "grep x /etc/passwd", "cat main.go /etc/passwd", "cat", "head", "grep", "grep package", "wc", "grep -e",
		"cat *.go", "head *.md", "wc -l *", "grep x *.go", "cat main.go*", "cat ?ain.go", "cat sub/../main.go", "cat 'sub/../main.go'", "cat /proc/self/environ", "cat /proc/1/environ",
		"cat -z main.go", "head -n main.go", "head -n -x main.go", "tail -n 99999999999999999999x main.go", "wc --files0-from=x", "wc --files0-from=-", "cat --help", "grep --help x",
		"head -n 5 main.go ../x", "cat .git/config/../../x", "wc -L /etc/passwd", "cat missing/../main.go", "cat $HOME/x", "cat 'x y' /etc/hosts",
		// (f) globs
		"ls /*", "ls ../*", "ls etclink/*", "ls */../*", "ls ~/*", "ls .*", "ls ..*", "ls [a-z]*", "ls *[a-z]", "ls {a,b}", "ls /etc/*.conf", "ls $HOME/*", "ls inlink/../*",
		"ls -R *", "ls -R /", "ls -I *.go", "ls *.go ../x", "ls ../", "ls etclink", "ls etclink/", "ls *\\*", "ls */../..", "ls ./../*", "ls sub/../../*", "ls sub/**/../*",
		"git log -- *.go", "git status *", "git diff *.go", "git log *", "cat *", "git add *", "git rm *", "echo *", "wc -l *.go", "grep x *", "head -n 1 *", "tail -n 1 *.go",
		// (g)
		"cd /etc && ls", "cd .. && ls", "cd etclink && ls", "cd sub/.. && ls", "cd ../ && ls", "cd ~ && ls", "cd - && ls", "cd && ls", "cd sub deep && ls", "cd * && ls", "cd sub*",
		"cd sub; ls", "cd sub || ls", "cd sub & ls", "cd sub | ls", "ls | cd sub", "cd sub >/dev/null && ls", "cd sub 2>&1 && ls", "cd nosuchdir && ls", "cd main.go && ls",
		"cd sub && cat ../main.go", "cd sub && ls ..", "cd sub && cat /etc/passwd", "cd sub/deep && cat ../a.go", "cd sub && cd .. && ls",
		"cd sub && git log -- ../main.go", "cd sub && ls /etc", "cd inlink && ls /", "cd sub && cd ../.. && ls", "cd sub && cd /", "cd $HOME && ls", "cd sub && ls $(pwd)",
		"git status && rm -rf x", "git status && curl x", "git status && make", "ls && ./x", "git status &&", "&& git status", "git status && && ls", "ls && ls ;", "pwd && sh",
		"cd sub && sh -c ls", "cd sub && ls && rm x", "cd sub && git -C .. status", "cd sub && git --git-dir=.. status", "git -C sub status && ls", "ls && git -c core.pager=x log",
	)
}

func TestARedirectFormIsTheWholeToken(t *testing.T) {
	dir := workspace(t)
	table(t, dir, agentturn.Allow, "git status 2>&1", "git status 2>&1 | head", "git log 2>/dev/null | head -n 2", "git status >/dev/null")
	table(t, dir, agentturn.Defer, "git status2>&1", "git status x2>&1", "git status 22>&1", "git status 2>&12", "git status 2>/dev/null2", "git status -2>&1")
}
