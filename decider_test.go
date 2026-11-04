package dax

import (
	"bufio"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentconsole/client"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// A call -p asks about when its input has ended was refused because
// nobody was there to answer, not by the person: the record names the
// host's rule as the decider, and the model is not told the user
// denied it.
func TestNobodyAnsweringIsNotThePersonsRefusal(t *testing.T) {
	sess, _ := sessionOver(t, &steps{parent: [][2]string{{"bash", `{"command":"touch ASKED"}`}}})
	f := &printFront{info: frontInfo{Name: "dax", Prompt: "go"}, in: bufio.NewScanner(strings.NewReader(""))}
	_ = f.run(t.Context(), sess, sess)
	r, err := client.RecordOf(sess.Recorder())
	if err != nil {
		t.Fatal(err)
	}
	rec, err := r.Read(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	var bys []string
	var told string
	for _, e := range rec.Path(rec.Leaf()) {
		switch e := e.(type) {
		case *agentsession.DecisionEntry:
			if e.Verdict != "hold" {
				bys = append(bys, e.Verdict+" by "+e.By)
			}
		case *agentsession.ItemEntry:
			if o, ok := e.Item.(*openresponses.FunctionCallOutput); ok {
				told = o.Output.Text
			}
		}
	}
	if len(bys) != 1 || !strings.HasSuffix(bys[0], " by policy") {
		t.Errorf("decisions after the question: %q, want one by policy", bys)
	}
	if strings.Contains(told, "user denied") || !strings.Contains(told, "nobody") {
		t.Errorf("the model was told %q, want that nobody was there to answer", told)
	}
}
