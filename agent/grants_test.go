package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
)

// grantPermit approves every call it is asked about with a grant of
// kind for the rule the session offers, and records what it was asked
// and offered.
func grantPermit(s *Session, kind GrantKind, asked, offered *[]string) Rules {
	return Rules{Permit: func(c *openresponses.FunctionCall, _ string) PermitAnswer {
		*asked = append(*asked, c.Arguments)
		o := s.GrantOffer(context.Background(), c.CallID)
		*offered = append(*offered, o.Rule)
		return PermitAnswer{Allow: true, Grant: kind, Rule: o.Rule}
	}}
}

// perPrompt is scripted with calls per prompt: the last user message
// that is not a note picks them.
type perPrompt map[string][][2]string

func (m perPrompt) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	var last string
	for _, it := range req.Input {
		if msg, ok := it.(*openresponses.Message); ok && msg.Role == openresponses.RoleUser {
			if _, ok := m[msg.Text()]; ok {
				last = msg.Text()
			}
		}
	}
	return (&scripted{calls: m[last]}).CreateStream(ctx, req, sink)
}

// "a" grants the offered rule for the session: a later call it covers
// runs unasked in this session, and a new session asks again.
func TestASessionGrantAnswersLaterCallsInThisSessionOnly(t *testing.T) {
	ctx := context.Background()
	model := perPrompt{"go": {{"bash", `{"command":"echo hello"}`}}, "again": {{"bash", `{"command":"echo hello world"}`}}}
	o := options(t, model)
	o.Policy = confirmPolicy(t)
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	var asked, offered []string
	if _, err := Drive(ctx, s, grantPermit(s, GrantSession, &asked, &offered), openresponses.UserText("go")); err != nil {
		t.Fatal(err)
	}
	if strings.Join(offered, ",") != "bash(echo hello:*)" {
		t.Fatalf("offered %q", offered)
	}
	asked = nil
	if _, err := Drive(ctx, s, grantPermit(s, GrantSession, &asked, &offered), openresponses.UserText("again")); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 0 {
		t.Errorf("a session grant still asked about %v", asked)
	}
	s.Close()

	// A new session in the same project asks again.
	s2, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := Drive(ctx, s2, grantPermit(s2, GrantOnce, &asked, &offered), openresponses.UserText("again")); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 {
		t.Errorf("a new session asked about %v, want the one call", asked)
	}
}

// "A" keeps the rule for the project: a new session in it runs the call
// unasked, and a session in another project asks.
func TestASavedGrantHoldsForTheProjectAlone(t *testing.T) {
	ctx := context.Background()
	o := options(t, &scripted{calls: [][2]string{{"bash", `{"command":"echo hello"}`}}})
	o.Policy = confirmPolicy(t)
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	var asked, offered []string
	if _, err := Drive(ctx, s, grantPermit(s, GrantSaved, &asked, &offered), openresponses.UserText("go")); err != nil {
		t.Fatal(err)
	}
	if o := s.GrantOffer(ctx, "none"); o.Rule != "" {
		t.Errorf("an offer for no pending call: %+v", o)
	}
	s.Close()

	asked = nil
	o.Streamer = &scripted{calls: [][2]string{{"bash", `{"command":"echo hello again"}`}}}
	s2, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Drive(ctx, s2, grantPermit(s2, GrantOnce, &asked, &offered), openresponses.UserText("go")); err != nil {
		t.Fatal(err)
	}
	s2.Close()
	if len(asked) != 0 {
		t.Errorf("a saved grant still asked in its project: %v", asked)
	}

	other := o
	other.Dir = filepath.Join(t.TempDir(), "other")
	write(t, filepath.Join(other.Dir, "README"), "x\n")
	other.Streamer = &scripted{calls: [][2]string{{"bash", `{"command":"echo hello again"}`}}}
	s3, err := New(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	if _, err := Drive(ctx, s3, grantPermit(s3, GrantOnce, &asked, &offered), openresponses.UserText("go")); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 {
		t.Errorf("another project asked about %v, want the one call", asked)
	}
}

// No grant is offered over a rule's ask (dax-coding's secret paths),
// nor for a line the analyzer does not read, and a rule dax did not
// offer is refused.
func TestNoGrantOverARulesAsk(t *testing.T) {
	ctx := context.Background()
	o := options(t, &scripted{calls: [][2]string{{"read", `{"path":".env"}`}, {"bash", `{"command":"echo $(id)"}`}, {"bash", `{"command":"echo hi"}`}}})
	o.Policy = confirmPolicy(t)
	write(t, filepath.Join(o.Dir, ".env"), "K=v\n")
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var offered []string
	var forged error
	_, err = Drive(ctx, s, Rules{Permit: func(c *openresponses.FunctionCall, _ string) PermitAnswer {
		offer := s.GrantOffer(ctx, c.CallID)
		offered = append(offered, c.Name+"="+offer.Rule)
		if strings.Contains(c.Arguments, "echo hi") {
			forged = s.Grant(ctx, c.CallID, GrantSession, "bash")
		}
		return PermitAnswer{Allow: false}
	}}, openresponses.UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(offered, ",") != "read=,bash=,bash=bash(echo hi:*)" {
		t.Errorf("offered %q", offered)
	}
	if forged == nil {
		t.Error("a rule dax did not offer was granted")
	}
}
