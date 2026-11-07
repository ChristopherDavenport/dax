package memory

import (
	"context"
	"fmt"
	"testing"

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
// after it, one waits for the person's next prompt; headless, none waits
// and the person is told once; a trivial run starts none. (#160)
func TestAnOpenWorldRunGetsProposeNotAuto(t *testing.T) {
	ctx := context.Background()
	mcp := extension.Run{Calls: []string{"mcp__web__fetch"}, OpenWorld: true}
	p := &passer{env: &passEnv{}, b: Background{Mode: BackgroundAuto}}
	p.after(ctx, mcp)
	if !p.due {
		t.Error("an open-world run under auto left no proposing pass due")
	}
	env := &passEnv{}
	h := &passer{env: env, b: Background{Mode: BackgroundAuto, Headless: true}}
	h.after(ctx, mcp)
	h.after(ctx, mcp)
	if h.due || len(env.logs) != 1 {
		t.Errorf("headless: due %v, notes %q; want none due and one note", h.due, env.logs)
	}
	q := &passer{env: &passEnv{}, b: Background{Mode: BackgroundPropose}}
	q.after(ctx, extension.Run{OutputTokens: 20})
	if q.due || q.runs != 0 {
		t.Errorf("a trivial run: due %v, runs %d", q.due, q.runs)
	}
}
