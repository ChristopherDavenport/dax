package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses/echo"
)

// A one-shot run (-p) tells the model nobody will reply to its last
// message, in a part of its own the record keeps with the instructions;
// a session someone answers is told nothing of it (#137).
func TestAOneShotRunIsToldNobodyWillReply(t *testing.T) {
	for _, oneShot := range []bool{true, false} {
		ctx := context.Background()
		o := options(t, &echo.Adapter{})
		o.OneShot = oneShot
		s, err := New(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		var part bool
		for _, p := range s.Kit.Parts() {
			if p.ID == PartOneShot {
				part = p.Source == "dax" && strings.Contains(p.Text, "nobody will reply")
			}
		}
		if part != oneShot {
			t.Errorf("one-shot %v: the part is there: %v", oneShot, part)
		}
		if _, err := promptOn(ctx, s, "hi", nil); err != nil {
			t.Fatal(err)
		}
		if data := string(projected(t, o, s)); strings.Contains(data, "nobody will reply") != oneShot {
			t.Errorf("one-shot %v: the record holds the line: %v", oneShot, !oneShot)
		}
	}
}
