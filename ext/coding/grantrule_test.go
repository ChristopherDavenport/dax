package coding

import (
	"encoding/json"
	"testing"

	"github.com/ChristopherDavenport/dax/extension"
)

// "Yes, always" offers a rule only where its prefix names what runs: a
// shell, an interpreter or a launcher runs whatever follows it, and a
// flag in second place is an option, not a subcommand, so none of them
// is offered (#146's review, M4).
func TestTheOfferedGrantIsNeverALauncher(t *testing.T) {
	ws, files := local(t, t.TempDir())
	offer := grantRule(extension.ToolEnv{Workspace: ws, Files: files})
	for line, want := range map[string]string{
		"go test ./...":                 "bash(go test:*)",
		"cd sub && go test ./...":       "bash(go test:*)",
		"make":                          "bash(make:*)",
		"npm run build":                 "bash(npm run:*)",
		"bash -c 'echo hi'":             "",
		"sh -c 'rm -rf x'":              "",
		"zsh script.sh":                 "",
		"/bin/sh -c id":                 "",
		"python -c 'import os'":         "",
		"python3 tool.py":               "",
		"python3.12 tool.py":            "",
		"node -e 'process.exit()'":      "",
		"node script.js":                "",
		"ruby x.rb":                     "",
		"perl -e 1":                     "",
		"env X=1 go test":               "",
		"/usr/bin/env go test":          "",
		"xargs rm":                      "",
		"sudo make install":             "",
		"nohup go run .":                "",
		"timeout 5 go test":             "",
		"nice go test":                  "",
		"find . -name x":                "",
		"find . -exec rm {} ;":          "",
		"git -c core.pager=x log":       "",
		"git --exec-path=/tmp status":   "",
		"go -C /tmp test":               "",
		"npx something":                 "",
		"exec go test":                  "",
		"eval echo hi":                  "",
		"command go test":               "",
		"awk 'BEGIN{system(\"id\")}'":   "",
		"go test ./... && go vet ./...": "",
	} {
		args, _ := json.Marshal(map[string]string{"command": line})
		if got := offer(t.Context(), "bash", args); got != want {
			t.Errorf("%s: offered %q, want %q", line, got, want)
		}
	}
}
