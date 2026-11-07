package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/ext/plan"
	"github.com/ChristopherDavenport/dax/policy"
)

// In plan mode write is denied, even over the user's own allow, and a
// command that would ask is refused unasked in plan's words, while
// exit_plan_mode asks; approving it moves the session to acceptEdits,
// recorded as the plan's approval, and to act_model, before the Resume,
// and the next call runs. (#147)
func TestPlanModeHandsOverToActingOnApproval(t *testing.T) {
	ctx := context.Background()
	const planText = "1. add retries to client.go\n2. go test ./..."
	o := options(t, &scripted{calls: [][2]string{
		{"write", `{"path":"a.go","content":"x"}`},
		{"bash", `{"command":"touch b"}`},
		{"exit_plan_mode", `{"plan":"` + strings.ReplaceAll(planText, "\n", `\n`) + `"}`},
		{"write", `{"path":"c.go","content":"x"}`},
	}})
	o.Policy = confirmPolicy(t)
	o.Policy.User.Allow = []string{"write"}
	o.Policy.Mode = policy.ModePlan
	o.Extensions = append(slices.Clone(o.Extensions), plan.New())
	o.ActModel = "act-model"
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var asked []string
	if _, err := promptOn(ctx, s, "add retries", func(c *openresponses.FunctionCall, _ string) bool {
		asked = append(asked, c.Name)
		return c.Name == plan.Tool
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(asked, []string{plan.Tool}) {
		t.Errorf("asked about %v, want exit_plan_mode alone", asked)
	}
	outs := outputs(s)
	if len(outs) != 4 || !strings.Contains(outs[0], "denied") || !strings.Contains(outs[1], "plan mode") || !strings.Contains(outs[1], policy.NoWorkAround) {
		t.Fatalf("the outputs: %q; want write denied, bash refused in plan's words", outs)
	}
	for _, f := range []string{"a.go", "b"} {
		if _, err := os.Stat(filepath.Join(o.Dir, f)); err == nil {
			t.Errorf("%s was made in plan mode", f)
		}
	}
	if _, err := os.Stat(filepath.Join(o.Dir, "c.go")); err != nil {
		t.Errorf("the write after the approval did not run: %v; outputs %q", err, outs)
	}
	if mode, _ := s.Mode(); mode != policy.ModeAcceptEdits {
		t.Errorf("mode %q after the approval, want acceptEdits", mode)
	}
	if m := s.Model(); m != "act-model" {
		t.Errorf("model %q after the approval, want act_model", m)
	}
	if !strings.Contains(string(projected(t, o, s)), `"reason":"plan approved`) {
		t.Error("the dax.mode entry does not say the plan was approved")
	}
}

// Outside plan mode exit_plan_mode is denied bare, asking no one; and
// Shift-Tab's cycle passes through plan. (#147)
func TestExitPlanModeIsDeniedOutsidePlanMode(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{policy.ModeDefault, policy.ModeAcceptEdits, policy.ModeDontAsk, policy.ModeBypass} {
		o := options(t, &scripted{calls: [][2]string{{"exit_plan_mode", `{"plan":"x"}`}}})
		o.Policy = confirmPolicy(t)
		o.Policy.Mode = mode
		o.Extensions = append(slices.Clone(o.Extensions), plan.New())
		s, err := New(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		var asked []string
		if _, err := promptOn(ctx, s, "go", asker(&asked)); err != nil {
			t.Fatal(err)
		}
		if outs := outputs(s); len(asked) != 0 || len(outs) != 1 || !strings.Contains(outs[0], "denied") {
			t.Errorf("%s: asked %v, outputs %q; want it denied bare", mode, asked, outs)
		}
		if m, _ := s.Mode(); m != mode {
			t.Errorf("%s: the mode moved to %s", mode, m)
		}
		s.Close()
	}
	if policy.NextMode(policy.ModeAcceptEdits) != policy.ModePlan {
		t.Error("Shift-Tab skips plan")
	}
}
