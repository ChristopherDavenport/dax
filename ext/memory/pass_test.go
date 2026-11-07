package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentturn/compact"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/extension"
)

// passEnv is the session as a pass sees it, but for its notes: a pass
// that would reach for the kit panics on the nil Env.
type passEnv struct {
	extension.Env
	logs []string
}

func (e *passEnv) Log(format string, args ...any) {
	e.logs = append(e.logs, fmt.Sprintf(format, args...))
}

// A run that reached an open world gets propose under auto: no pass runs
// after it, one waits for the person's next prompt; a trivial run
// starts none. (#160)
func TestAnOpenWorldRunGetsProposeNotAuto(t *testing.T) {
	ctx := context.Background()
	mcp := extension.Run{Calls: []string{"mcp__web__fetch"}, OpenWorld: true}
	p := &passer{env: &passEnv{}, b: Background{Mode: BackgroundAuto}}
	p.after(ctx, mcp)
	if !p.due {
		t.Error("an open-world run under auto left no proposing pass due")
	}
	q := &passer{env: &passEnv{}, b: Background{Mode: BackgroundPropose}}
	q.after(ctx, extension.Run{OutputTokens: 20})
	if q.due || q.runs != 0 {
		t.Errorf("a trivial run: due %v, runs %d", q.due, q.runs)
	}
}

// Under -p and dax serve (Headless) no pass runs at all, auto or
// propose: none after the run, none due at the next prompt, and the
// person is told once. A pass that ran would reach for the kit, which
// the nil Env does not have. (#160, the decision's addendum)
func TestNoPassRunsHeadless(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{BackgroundAuto, BackgroundPropose} {
		t.Run(mode, func(t *testing.T) {
			env := &passEnv{}
			h := &passer{env: env, b: Background{Mode: mode, Headless: true}}
			h.after(ctx, extension.Run{OutputTokens: 20}) // trivial: no note
			h.after(ctx, extension.Run{Calls: []string{"ls"}})
			h.after(ctx, extension.Run{Calls: []string{"mcp__web__fetch"}, OpenWorld: true})
			h.beforePrompt(ctx)
			if h.due || h.running || len(env.logs) != 1 {
				t.Errorf("due %v, running %v, notes %q; want none due and one note", h.due, h.running, env.logs)
			}
		})
	}
}

// The pass's token cap holds for the request about to go, not only for
// those that went: one whose input would reach the cap is refused, and
// another may write no more output than is left. (#160)
func TestAPassRequestStaysWithinTheCap(t *testing.T) {
	ctx := context.Background()
	big := openresponses.Request{Input: openresponses.Items{openresponses.UserText(strings.Repeat("x", 4000))}}
	b := &tokenBudget{max: 1000, used: 400}
	if err := b.check(ctx, &big); !errors.Is(err, errBudget) {
		t.Errorf("a request whose input reaches the cap: %v, want refused", err)
	}
	small := openresponses.Request{Input: openresponses.Items{openresponses.UserText("remember tabs")}}
	if err := b.check(ctx, &small); err != nil {
		t.Fatal(err)
	}
	in := compact.Estimate(small.Input) + compact.EstimateSettings(small)
	if small.MaxOutputTokens == nil || b.used+in+*small.MaxOutputTokens > b.max {
		t.Errorf("max output %v with %d used and %d in: past the cap of %d", small.MaxOutputTokens, b.used, in, b.max)
	}
	less := 10
	own := openresponses.Request{Input: small.Input, MaxOutputTokens: &less}
	if err := b.check(ctx, &own); err != nil || *own.MaxOutputTokens != 10 {
		t.Errorf("a smaller limit of the request's own: %v, %v", *own.MaxOutputTokens, err)
	}
}
