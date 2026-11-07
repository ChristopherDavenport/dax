package agent

import (
	"errors"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentsmd"
)

// The AGENTS.md part a caller's request carries is the kit's without
// the person's own files: the project's kept, the part and its
// separator gone when nothing is left, and a request still naming a
// file of theirs (a guard rewrote the part) refused.
func TestACallersAgentsMDLeavesOutThePersons(t *testing.T) {
	mine := agentsmd.File{Path: "/home/me/.dax/AGENTS.md", Content: "personal\n"}
	proj := agentsmd.File{Path: "AGENTS.md", Content: "project\n"}
	both := newCallerMD([]agentsmd.File{mine, proj}, []string{"/home/me/.dax/./AGENTS.md"})
	in := "prompt" + agentkit.Separator + both.all
	got, err := both.instructions(in)
	if err != nil || strings.Contains(got, "personal") || !strings.Contains(got, "project") {
		t.Errorf("both: %q, %v", got, err)
	}
	alone := newCallerMD([]agentsmd.File{mine}, []string{mine.Path})
	got, err = alone.instructions("prompt" + agentkit.Separator + alone.all + agentkit.Separator + "memory")
	if err != nil || got != "prompt"+agentkit.Separator+"memory" {
		t.Errorf("alone: %q, %v", got, err)
	}
	if ps := alone.parts([]agentkit.Part{{ID: "product", Text: "prompt"}, {ID: agentsmd.PartID, Text: alone.all}}); len(ps) != 1 {
		t.Errorf("parts %+v, want the AGENTS.md part left out", ps)
	}
	rewritten := strings.Replace(both.all, "personal", "[redacted]", 1)
	if _, err := both.instructions("prompt" + agentkit.Separator + rewritten); !errors.Is(err, errCallerMD) {
		t.Errorf("a rewritten part: %v, want the request refused", err)
	}
	none := newCallerMD([]agentsmd.File{proj}, []string{mine.Path})
	if got, err := none.instructions("x"); err != nil || got != "x" {
		t.Errorf("no file of the person's: %q, %v", got, err)
	}
}
