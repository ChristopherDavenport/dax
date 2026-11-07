package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

// The model hears of a mode switch at the next request, whatever starts
// it: a run a steer started through Deliver as well as a prompt (#145's
// review, L4).
func TestTheModelHearsOfASwitchInARunADeliveryStarts(t *testing.T) {
	ctx := context.Background()
	m := &told{}
	o := options(t, m)
	o.Policy = confirmPolicy(t)
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.SetMode(ctx, "acceptEdits"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Deliver(ctx, openresponses.UserText("go")); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); s.State().Running; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the run never ended")
		}
	}
	m.mu.Lock()
	devs := strings.Join(m.devs, "\n")
	m.mu.Unlock()
	if strings.Count(devs, "now acceptEdits") != 1 {
		t.Errorf("the model was told %q, want the switch once", devs)
	}
}

// A switch to a looser mode never leaves a moment in which an A2A
// caller's call is decided under the looser rules without the floor:
// the mode the floor reads is in force before the engine's policy is
// (#145's review, R2-M1).
func TestNoPeerCallSlipsThroughASwitch(t *testing.T) {
	ctx := context.Background()
	o := options(t, &scripted{})
	o.Policy = confirmPolicy(t)
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	bash, ok := s.Kit.LookupTool("bash")
	if !ok {
		t.Fatal("no bash")
	}
	args := `{"command":"touch PEER"}`
	call := &openresponses.FunctionCall{Name: "bash", Arguments: args, CallID: "c1"}
	info := agentturn.ToolCallInfo{Call: call, Tool: bash, Args: json.RawMessage(args), Batch: []*openresponses.FunctionCall{call}}
	peer := context.WithValue(ctx, peerKey{}, "A2A caller alice")
	var during agentturn.ToolAction
	s.modes.swapped = func() {
		v, err := s.Kit.Engine().Would(peer, info)
		if err != nil {
			t.Error(err)
		}
		during = v.Action
	}
	// Into bypass, and out of it to a stricter mode: either way the
	// swap's moment holds the peer to default.
	for _, mode := range []string{policy.ModeBypass, policy.ModeDontAsk, policy.ModeAcceptEdits, policy.ModeDefault} {
		during = agentturn.Allow
		if _, err := s.SetMode(ctx, mode); err != nil {
			t.Fatal(err)
		}
		if during != agentturn.Defer {
			t.Errorf("into %s: a peer's bash decided as the switch took effect: %v, want asked", mode, during)
		}
	}
}
