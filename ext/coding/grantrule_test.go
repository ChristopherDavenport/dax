package coding

import (
	"encoding/json"
	"testing"

	"github.com/ChristopherDavenport/dax/extension"
)

// "Yes, always" offers a prefix rule only for the programs and
// subcommands on grantPrefixes, known not to run whatever follows
// them; any other line is offered as it is, exactly, so the grant
// allows that line again and nothing else (#146's review, M4 and
// R2-M2: shells, interpreters, launchers, wrappers with a subcommand
// that runs a command, and programs with a command-running flag later
// in the line).
func TestTheOfferedGrantIsAPrefixOnlyFromTheAllowlist(t *testing.T) {
	ws, files := local(t, t.TempDir())
	offer := grantRule(extension.ToolEnv{Workspace: ws, Files: files})
	exact := func(line string) string { return "bash(" + line + ")" }
	tests := map[string]string{
		// The allowlist: a prefix.
		"go test ./...":           "bash(go test:*)",
		"cd sub && go test ./...": "bash(go test:*)",
		"go build ./cmd/dax":      "bash(go build:*)",
		"go vet ./...":            "bash(go vet:*)",
		"make":                    "bash(make)",
		"make check":              "bash(make check:*)",
		"npm test":                "bash(npm test:*)",
		"cargo test --lib":        "bash(cargo test:*)",
		"pytest tests/a_test.py":  "bash(pytest:*)",
		"ls src":                  "bash(ls:*)",
		// Two allowlisted prefixes in one line: no one rule covers it.
		"go test ./... && go vet ./...": "",
		// A glob in the line would be a pattern in the rule.
		"echo *.go": "",
	}
	// Everything else: the line exactly, never a prefix.
	for _, line := range []string{
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
		"rsync a b -e sh", "go test -exec sh ./...",
	} {
		tests[line] = exact(line)
	}
	tests["go test -exec sh ./..."] = "bash(go test:*)" // the prefix's own flags are the person's to weigh; see README
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
