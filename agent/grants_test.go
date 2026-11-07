package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/policy"
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
	model := perPrompt{"go": {{"bash", `{"command":"echo hello"}`}}, "again": {{"bash", `{"command":"echo hello"}`}}}
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
	if strings.Join(offered, ",") != "bash(echo hello)" {
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
	o.Streamer = &scripted{calls: [][2]string{{"bash", `{"command":"echo hello"}`}}}
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
	other.Streamer = &scripted{calls: [][2]string{{"bash", `{"command":"echo hello"}`}}}
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
	if strings.Join(offered, ",") != "read=,bash=,bash=bash(echo hi)" {
		t.Errorf("offered %q", offered)
	}
	if forged == nil {
		t.Error("a rule dax did not offer was granted")
	}
}

// A grant is a person's: an approval a rule gave (By not ByHuman)
// makes none, so a controller's rules cannot keep a rule for the
// session or the project (#146's review, L1).
func TestARulesApprovalMakesNoGrant(t *testing.T) {
	ctx := context.Background()
	model := perPrompt{"go": {{"bash", `{"command":"echo hello"}`}}, "again": {{"bash", `{"command":"echo hello"}`}}}
	o := options(t, model)
	o.Policy = confirmPolicy(t)
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	byRule := Rules{Permit: func(c *openresponses.FunctionCall, _ string) PermitAnswer {
		o := s.GrantOffer(ctx, c.CallID)
		return PermitAnswer{Allow: true, By: "policy", Grant: GrantSession, Rule: o.Rule}
	}}
	if _, err := Drive(ctx, s, byRule, openresponses.UserText("go")); err != nil {
		t.Fatal(err)
	}
	var asked, offered []string
	if _, err := Drive(ctx, s, grantPermit(s, GrantOnce, &asked, &offered), openresponses.UserText("again")); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 {
		t.Errorf("asked about %v after a rule's approval, want the call: the rule made a grant", asked)
	}
}

// exploring is perPrompt for the main agent, whose task child runs
// child's calls.
type exploring struct {
	perPrompt
	child [][2]string
}

func (m exploring) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	if strings.Contains(req.Instructions, "You are a sub-agent of dax") {
		return (&scripted{calls: m.child}).CreateStream(ctx, req, sink)
	}
	return m.perPrompt.CreateStream(ctx, req, sink)
}

// A grant the person made for the session reaches its sub-agents' calls
// too (#146's review, L3), in dontAsk as well; a sub-agent's call it
// does not cover is still asked.
func TestASessionGrantReachesTheSubagents(t *testing.T) {
	for _, mode := range []string{"default", "dontAsk"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			model := exploring{
				perPrompt: perPrompt{"go": {{"bash", `{"command":"touch a"}`}}, "look": {{"task", `{"input":"make b"}`}}},
				child:     [][2]string{{"bash", `{"command":"touch a"}`}, {"bash", `{"command":"touch x"}`}},
			}
			o := withAgents(options(t, model), "")
			o.Policy = confirmPolicy(t)
			s, err := New(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			var asked, offered []string
			if _, err := Drive(ctx, s, grantPermit(s, GrantSession, &asked, &offered), openresponses.UserText("go")); err != nil {
				t.Fatal(err)
			}
			if _, err := s.SetMode(ctx, mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(o.Dir, "a")); err != nil {
				t.Fatal(err)
			}
			asked = nil
			rules := rulesOf(func(c *openresponses.FunctionCall, _ string) bool { asked = append(asked, c.Arguments); return false })
			if _, err := Drive(ctx, s, rules, openresponses.UserText("look")); err != nil {
				t.Fatal(err)
			}
			want := 1
			if mode == "dontAsk" {
				want = 0
			}
			if len(asked) != want || slices.ContainsFunc(asked, func(a string) bool { return strings.Contains(a, "touch a") }) {
				t.Errorf("asked %q: want the child's touch a under the session's grant, its touch x asked in default", asked)
			}
			if _, err := os.Stat(filepath.Join(o.Dir, "a")); err != nil {
				t.Errorf("the child's call the grant covers did not run: %v", err)
			}
			if _, err := os.Stat(filepath.Join(o.Dir, "x")); err == nil {
				t.Error("the child's call no grant covers ran")
			}
		})
	}
}

// A sub-agent's asked call offers "yes, always" in the person's session
// only when the sub-agent is the person's: an A2A caller's sub-agent,
// whose question an escalation may put to the person, is offered no
// grant, since its conversation is not the session's. (#160)
func TestACallersSubagentIsOfferedNoGrant(t *testing.T) {
	ctx := context.Background()
	o := options(t, &scripted{})
	o.Policy = confirmPolicy(t)
	var env extension.Env
	o.Extensions = append(slices.Clone(o.Extensions), extension.Extension{Name: "probe",
		Kit: func(e extension.Env) ([]agentkit.Option, error) { env = e; return nil, nil }})
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	bash, ok := s.Kit.LookupTool("bash")
	if !ok {
		t.Fatal("no bash")
	}
	decide := env.ChildPolicy("task")
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"the person's", ctx, "bash(echo hello)"},
		{"a caller's", context.WithValue(ctx, peerKey{}, "A2A caller alice"), ""},
	} {
		args := `{"command":"echo hello"}`
		call := &openresponses.FunctionCall{Name: "bash", Arguments: args, CallID: "c-" + tc.name}
		d, err := decide(tc.ctx, agentturn.ToolCallInfo{Call: call, Tool: bash, Args: json.RawMessage(args), Batch: []*openresponses.FunctionCall{call}})
		if err != nil || d == nil || d.Action != agentturn.Defer {
			t.Fatalf("%s: %+v, %v; want asked", tc.name, d, err)
		}
		q := agentturn.Question{Call: &agentturn.AskedCall{CallID: call.CallID, Name: "bash", Args: json.RawMessage(args)}}
		if got := s.QuestionOffer(ctx, q).Rule; got != tc.want {
			t.Errorf("%s sub-agent's question offers %q, want %q", tc.name, got, tc.want)
		}
	}
}

// /permissions lists a sub-agent's own grant for the session (the
// memory pass's "yes, always") with the scope it decides under, held
// or not in the engine at the moment. (#160)
func TestPermissionsListASubagentsOwnGrant(t *testing.T) {
	ctx := context.Background()
	o := options(t, &scripted{})
	o.Policy = confirmPolicy(t)
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	scope := s.Kit.GrantScope(ctx) + "/pass"
	if err := s.grantUnder(ctx, scope, "memory_save"); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, g := range s.Info().Policy.Grants {
		if g.Scope == scope && g.Source == policy.SourceSession && slices.Equal(g.Allow, []string{"memory_save"}) {
			found = true
		}
	}
	if !found {
		t.Errorf("the grants listed %+v lack the sub-agent's memory_save under %s", s.Info().Policy.Grants, scope)
	}
}
