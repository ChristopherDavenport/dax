package agent

import (
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentpolicy"
)

// A person's refusal tells the model not to get the same result another
// way, and is the same words whether it ends the run or not; only the
// kind differs (#150).
func TestARefusalSaysNotToWorkAroundIt(t *testing.T) {
	goOn, stop := Refusal("c1", "not that file", false), Refusal("c1", "not that file", true)
	for _, a := range []struct {
		name string
		text string
		by   string
	}{{"go on", goOn.Output.Output.Text, goOn.By}, {"stop", stop.Output.Output.Text, stop.By}} {
		if !strings.Contains(a.text, "Do not get the same result another way (another tool, a script, another path); if it is still needed, say so and let the user decide.") {
			t.Errorf("%s: the model is told %q", a.name, a.text)
		}
		if !strings.HasSuffix(a.text, " Reason: not that file") {
			t.Errorf("%s: the reason is not carried: %q", a.name, a.text)
		}
		if a.by != agentpolicy.ByHuman {
			t.Errorf("%s: by %q, want a person's", a.name, a.by)
		}
	}
	if goOn.Output.Output.Text != stop.Output.Output.Text {
		t.Errorf("the two refusals say different things: %q and %q", goOn.Output.Output.Text, stop.Output.Output.Text)
	}
	if goOn.Terminate || !stop.Terminate {
		t.Errorf("terminate: go on %v, stop %v", goOn.Terminate, stop.Terminate)
	}
}
