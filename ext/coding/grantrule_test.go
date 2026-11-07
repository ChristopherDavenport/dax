package coding

import (
	"encoding/json"
	"testing"

	"github.com/ChristopherDavenport/dax/extension"
)

// "Yes, always" offers a prefix rule only for the listing tools on
// grantPrefixes, which no flag makes run a command; any other line is
// offered as it is, exactly, so the grant allows that line again and
// nothing else (#146's review, M4, R2-M2 and R3-M1: shells,
// interpreters, launchers, wrappers with a subcommand that runs a
// command, and build tools with a command-running flag or variable:
// go test -exec, make --eval).
func TestTheOfferedGrantIsAPrefixOnlyFromTheAllowlist(t *testing.T) {
	ws, files := local(t, t.TempDir())
	offer := grantRule(extension.ToolEnv{Workspace: ws, Files: files})
	exact := func(line string) string { return "bash(" + line + ")" }
	tests := map[string]string{
		// The allowlist: a prefix.
		"ls src":           "bash(ls:*)",
		"cd sub && ls -la": "bash(ls:*)",
		"wc -l a.go":       "bash(wc:*)",
		"du -sh .":         "bash(du:*)",
		"df -h":            "bash(df:*)",
		// Two commands in one line: no one rule covers it.
		"go test ./... && go vet ./...": "",
		"cd sub && go test ./...":       "",
		// A glob in the line would be a pattern in the rule.
		"echo *.go": "",
	}
	// Everything else: the line exactly, never a prefix.
	for _, line := range []string{
		// Build and test tools run what a later flag or variable names.
		"go test ./...", "go build ./cmd/dax", "go vet ./...", "go test -exec sh ./...", "go test -toolexec sh ./...",
		"go build -toolexec sh .", "go vet -vettool=/tmp/x ./...", "make", "make check", "make check --eval=x",
		"make check SHELL=/tmp/x", "make check CC=/tmp/x", "make -f /tmp/x.mk", "npm test", "pnpm test", "yarn test",
		"cargo test --lib", "pytest tests/a_test.py", "tree -o out.txt",
		"bash -c 'echo hi'", "sh -c 'rm -rf x'", "python -c 'import os'", "node -e 'process.exit()'",
		"env X=1 go test", "xargs rm", "sudo make install", "nohup go run .", "timeout 5 go test",
		"find . -name x", "git -c core.pager=x log", "go -C /tmp test", "go run .", "go generate ./...",
		"make -f /tmp/x.mk", "make X=1", "npx something", "awk 'BEGIN{print 1}'",
		"uv run python -c 'print(1)'", "poetry run pytest", "pipenv run x", "conda run x",
		"npm exec x", "npm run build", "pnpm exec x", "pnpm dlx x", "yarn dlx x", "yarn exec x",
		"bundle exec rake", "docker run alpine", "docker exec box sh", "podman run alpine", "kubectl exec pod -- sh",
		"nix run nixpkgs#hello", "nix-shell -p x", "mise exec -- x", "asdf exec x", "direnv exec . x", "rbenv exec x", "pyenv exec x",
		"git rebase main", "git rebase --exec sh main", "git bisect run sh", "git submodule foreach sh",
		"tmux new -d x", "screen -dm x", "expect script.exp", "unshare -r sh", "nsenter -t 1 sh",
		"firejail sh", "bwrap --bind / / sh", "taskset 1 sh", "chrt 1 sh", "numactl sh", "fakeroot sh",
		"xvfb-run sh", "dbus-run-session sh", "vim notes.txt -c '!id'", "tar xf a.tar --to-command=sh",
		"rsync a b -e sh",
	} {
		tests[line] = exact(line)
	}
	// A quoted line is not offered: the rule matches its words joined,
	// which would show the person another line than they approved.
	for _, line := range []string{"bash -c 'echo hi'", "sh -c 'rm -rf x'", "python -c 'import os'", "node -e 'process.exit()'",
		"awk 'BEGIN{print 1}'", "uv run python -c 'print(1)'", "vim notes.txt -c '!id'"} {
		tests[line] = ""
	}
	// A line the analyzer does not read is not offered.
	tests["nix run nixpkgs#hello"] = ""
	for line, want := range tests {
		args, _ := json.Marshal(map[string]string{"command": line})
		if got := offer(t.Context(), "bash", args); got != want {
			t.Errorf("%s: offered %q, want %q", line, got, want)
		}
	}
}
