package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/policy"
)

// told is a scripted model that keeps the developer messages it is
// sent.
type told struct {
	scripted
	mu   sync.Mutex
	devs []string
}

func (m *told) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	m.mu.Lock()
	for _, it := range req.Input {
		if msg, ok := it.(*openresponses.Message); ok && msg.Role == openresponses.RoleDeveloper {
			m.devs = append(m.devs, msg.Text())
		}
	}
	m.mu.Unlock()
	return m.scripted.CreateStream(ctx, req, sink)
}

// asker records which calls were asked about, and approves none.
func asker(asked *[]string) func(*openresponses.FunctionCall, string) bool {
	return func(c *openresponses.FunctionCall, _ string) bool {
		*asked = append(*asked, c.Name)
		return false
	}
}

// /mode acceptEdits lets write run unasked while bash still asks, and a
// write to a protected path still asks; /mode default puts the asks
// back. The model is told of each switch at the next prompt, and the
// record holds one dax.mode entry per switch.
func TestAModeSwitchChangesWhatAsks(t *testing.T) {
	ctx := context.Background()
	m := &told{scripted: scripted{calls: [][2]string{{"write", `{"path":"a.txt","content":"x"}`}, {"write", `{"path":".git/hooks/pre-commit","content":"x"}`}, {"bash", `{"command":"touch b"}`}}}}
	o := options(t, m)
	o.Policy = confirmPolicy(t)
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if mode, _ := s.Mode(); mode != policy.ModeDefault {
		t.Fatalf("starts in %q", mode)
	}
	if pending, err := s.SetMode(ctx, "acceptEdits"); err != nil || pending {
		t.Fatalf("SetMode: %v, pending %v", err, pending)
	}
	var asked []string
	if _, err := promptOn(ctx, s, "go", asker(&asked)); err != nil {
		t.Fatal(err)
	}
	if strings.Join(asked, ",") != "write,bash" {
		t.Errorf("acceptEdits asked about %v, want the protected write and bash", asked)
	}
	if _, err := os.Stat(filepath.Join(o.Dir, "a.txt")); err != nil {
		t.Errorf("acceptEdits did not write: %v", err)
	}

	if _, err := s.SetMode(ctx, "default"); err != nil {
		t.Fatal(err)
	}
	asked = nil
	if _, err := promptOn(ctx, s, "again", asker(&asked)); err != nil {
		t.Fatal(err)
	}
	if strings.Join(asked, ",") != "write,write,bash" {
		t.Errorf("default asked about %v, want every write and bash", asked)
	}
	m.mu.Lock()
	devs := strings.Join(m.devs, "\n")
	m.mu.Unlock()
	if !strings.Contains(devs, "now acceptEdits") || !strings.Contains(devs, "now default") {
		t.Errorf("the model was not told: %q", devs)
	}
	data := string(projected(t, o, s))
	if n := strings.Count(data, `"dax.mode"`); n != 2 {
		t.Errorf("%d dax.mode entries, want 2", n)
	}
	if !strings.Contains(data, "replaced the policy: mode acceptEdits") {
		t.Error("the policy's replacement is not recorded")
	}
}

// A session started in bypassPermissions (-no-policy's old meaning)
// asks nothing and can leave it.
func TestBypassCanBeLeft(t *testing.T) {
	ctx := context.Background()
	o := options(t, &scripted{calls: [][2]string{{"bash", `{"command":"touch b"}`}}})
	o.Policy = confirmPolicy(t)
	o.Policy.Mode = policy.ModeBypass
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	var asked []string
	if _, err := promptOn(ctx, s, "go", asker(&asked)); err != nil || len(asked) != 0 {
		t.Fatalf("bypass asked %v (%v)", asked, err)
	}
	if _, err := os.Stat(filepath.Join(o.Dir, "b")); err != nil {
		t.Errorf("bypass did not run bash: %v", err)
	}
	if _, err := s.SetMode(ctx, "default"); err != nil {
		t.Fatal(err)
	}
	if _, err := promptOn(ctx, s, "again", asker(&asked)); err != nil || strings.Join(asked, ",") != "bash" {
		t.Fatalf("after leaving bypass asked %v (%v)", asked, err)
	}
	data := string(projected(t, o, s))
	if n := strings.Count(data, `"dax.mode"`); n != 2 {
		t.Errorf("%d dax.mode entries, want the start's and the switch's", n)
	}
}

// A switch asked for during a run waits for the run's next turn: the
// call already decided is not decided again, and the next turn's is
// decided under the new mode.
func TestAModeAskedForDuringARunStartsAtTheNextTurn(t *testing.T) {
	ctx := context.Background()
	o := options(t, &scripted{calls: [][2]string{{"write", `{"path":"a.txt","content":"x"}`}, {"write", `{"path":"b.txt","content":"x"}`}}})
	o.Policy = confirmPolicy(t)
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	var pending bool
	var perr error
	defer s.Subscribe(func(ctx context.Context, ev agentturn.Event) error {
		if _, ok := ev.(*agentturn.ToolEnd); ok {
			once.Do(func() { pending, perr = s.SetMode(ctx, "acceptEdits") })
		}
		return nil
	})()
	var asked []string
	approve := func(c *openresponses.FunctionCall, _ string) bool { asked = append(asked, c.Name); return true }
	if _, err := promptOn(ctx, s, "go", approve); err != nil {
		t.Fatal(err)
	}
	if perr != nil || !pending {
		t.Fatalf("SetMode during the run: pending %v, %v", pending, perr)
	}
	if len(asked) != 1 {
		t.Errorf("asked %v, want only the first write", asked)
	}
	if mode, p := s.Mode(); mode != policy.ModeAcceptEdits || p != "" {
		t.Errorf("mode %q pending %q after the run", mode, p)
	}
}

// dontAsk refuses, without asking anyone, what the rules would ask
// about, the model told not to work around it, and runs what they
// allow; the rules themselves are not rewritten (#145's review, M1).
func TestDontAskRefusesWhatWouldAsk(t *testing.T) {
	ctx := context.Background()
	o := options(t, &scripted{calls: [][2]string{{"bash", `{"command":"touch b"}`}, {"ls", `{}`}}})
	o.Policy = confirmPolicy(t)
	o.Policy.Mode = policy.ModeDontAsk
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	var asked []string
	if _, err := promptOn(ctx, s, "go", asker(&asked)); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 0 {
		t.Errorf("dontAsk asked about %v", asked)
	}
	if _, err := os.Stat(filepath.Join(o.Dir, "b")); err == nil {
		t.Error("dontAsk ran bash")
	}
	var outs []string
	for _, it := range s.Agent().State().Transcript {
		if out, ok := it.(*openresponses.FunctionCallOutput); ok {
			outs = append(outs, out.Output.String())
		}
	}
	if len(outs) != 2 || !strings.Contains(outs[0], "dontAsk") || !strings.Contains(outs[0], policy.NoWorkAround) {
		t.Errorf("the outputs: %q, want bash refused in dontAsk's words and ls run", outs)
	}
	if p := s.Kit.Engine().Policy(); len(p.Deny) != 0 || func() bool { a, _ := p.Default.Action(); return a != agentturn.Defer }() {
		t.Errorf("dontAsk rewrote the rules: deny %v, default %v", p.Deny, p.Default)
	}
}
