package coding

import (
	"encoding/json"
	"testing"

	"github.com/ChristopherDavenport/agentturn"

	"github.com/ChristopherDavenport/dax/policy"
)

func writeArgs(path string) string {
	b, _ := json.Marshal(map[string]string{"path": path, "content": "x"})
	return string(b)
}

// In acceptEdits a write or an edit runs unasked, but for a path that
// changes what runs or is trusted, or looks like a secret; a command
// still asks, a compound one and one that writes through a redirect
// included, and a deny stays.
func TestAcceptEditsRunsEditsAndAsksTheRest(t *testing.T) {
	s := defaults
	s.Mode = policy.ModeAcceptEdits
	s.User = Rules{Deny: []string{"write(vendor/**)"}}
	for _, tc := range []struct {
		tool, args string
		want       agentturn.ToolAction
	}{
		{"write", writeArgs("main.go"), agentturn.Allow},
		{"edit", `{"path":"pkg/a.go","old_string":"a","new_string":"b"}`, agentturn.Allow},
		{"write", writeArgs(".git/hooks/pre-commit"), agentturn.Defer},
		{"write", writeArgs(".GIT/config"), agentturn.Defer},
		{"write", writeArgs("sub/.dax/config.json"), agentturn.Defer},
		{"write", writeArgs(".github/workflows/ci.yml"), agentturn.Defer},
		{"write", writeArgs(".envrc"), agentturn.Defer},
		{"write", writeArgs("config/.env.local"), agentturn.Defer},
		{"write", writeArgs("certs/server.pem"), agentturn.Defer},
		{"write", writeArgs("vendor/x.go"), agentturn.Block},
		{"bash", bash("echo x > main.go"), agentturn.Defer},
		{"bash", bash("go test ./... && echo ok"), agentturn.Defer},
		{"bash", bash("git status"), agentturn.Allow},
	} {
		if got, why := decide(t, s, tc.tool, tc.args); got != tc.want {
			t.Errorf("acceptEdits %s %s = %v (%s), want %v", tc.tool, tc.args, got, why, tc.want)
		}
	}
	// In the default mode the same write asks.
	if got, _ := decide(t, defaults, "write", writeArgs("main.go")); got != agentturn.Defer {
		t.Errorf("default write = %v, want it to ask", got)
	}
}
