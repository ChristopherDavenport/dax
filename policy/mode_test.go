package policy

import (
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentturn"
)

// Each mode is the same rules made stricter or looser: acceptEdits adds
// the extensions' mode rules, dontAsk keeps default's (the session
// refuses what they ask), and
// bypassPermissions asks about nothing. A deny stays in every mode.
func TestModes(t *testing.T) {
	sh := acme(Rules{Allow: []string{"rollback"}, Deny: []string{"deploy(prod)"}, Ask: []string{"deploy(stage)"}})
	sh.Modes = map[string]Rules{ModeAcceptEdits: {Allow: []string{"deploy"}, Ask: []string{"deploy(*secret*)"}}}
	base := Settings{Builtin: true, Fallback: "ask", Shipped: []Shipped{sh}, User: Rules{Allow: []string{"deploy(dev)"}}}
	tests := []struct {
		mode, env string
		want      agentturn.ToolAction
	}{
		{ModeDefault, "x", agentturn.Defer},
		{ModeDefault, "dev", agentturn.Allow},
		{ModeDefault, "prod", agentturn.Block},
		{ModeAcceptEdits, "x", agentturn.Allow},
		{ModeAcceptEdits, "my-secret", agentturn.Defer},
		{ModeAcceptEdits, "stage", agentturn.Defer},
		{ModeAcceptEdits, "prod", agentturn.Block},
		// dontAsk's rules are default's; the session refuses what they
		// would ask (agent's TestDontAskRefusesWhatWouldAsk).
		{ModeDontAsk, "x", agentturn.Defer},
		{ModeDontAsk, "stage", agentturn.Defer},
		{ModeDontAsk, "dev", agentturn.Allow},
		{ModeDontAsk, "prod", agentturn.Block},
		{ModeBypass, "x", agentturn.Allow},
		{ModeBypass, "stage", agentturn.Allow},
		{ModeBypass, "prod", agentturn.Block},
	}
	for _, tc := range tests {
		s := base
		s.Mode = tc.mode
		if v := verdict(t, s, "deploy", env(tc.env)); v.Action != tc.want {
			t.Errorf("%s deploy(%s) = %v (%s), want %v", tc.mode, tc.env, v.Action, v.Reason, tc.want)
		}
	}
	// A mode no one knows is an error.
	s := base
	s.Mode = "yolo"
	if _, err := Build(s); err == nil {
		t.Error("an unknown mode built")
	}
	// A mode rule that names another extension's tool is refused, as a
	// shipped rule is.
	bad := acme(Rules{})
	bad.Modes = map[string]Rules{ModeAcceptEdits: {Allow: []string{"bash"}}}
	if _, err := Build(Settings{Mode: ModeAcceptEdits, Shipped: []Shipped{bad}}); err == nil || !strings.Contains(err.Error(), "not one of the extension's tools") {
		t.Errorf("a mode rule for another's tool: %v", err)
	}
}

// A carve-out lifts only what its own list holds, in every mode: a
// user's ask carve-out never cancels a deny, so a call denied in
// default is denied in each mode (#145's review, M1).
func TestACarveOutNeverLiftsADeny(t *testing.T) {
	s := Settings{Builtin: true, Fallback: "ask", Shipped: []Shipped{acme(Rules{Deny: []string{"deploy(prod)"}})},
		User: Rules{Allow: []string{"deploy"}, Ask: []string{"deploy(!prod)"}}}
	for _, mode := range Modes {
		s.Mode = mode
		if v := verdict(t, s, "deploy", env("prod")); v.Action != agentturn.Block {
			t.Errorf("%s: deploy(prod) = %v (%s), want denied", mode, v.Action, v.Reason)
		}
	}
}

func TestParseModeAndStricter(t *testing.T) {
	for in, want := range map[string]string{"default": ModeDefault, "acceptedits": ModeAcceptEdits, "acceptEdits": ModeAcceptEdits, "dontAsk": ModeDontAsk, "bypassPermissions": ModeBypass, "bypass": ModeBypass} {
		if got, err := ParseMode(in); err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ParseMode("plan"); err == nil {
		t.Error("plan parsed; it is dax#147's")
	}
	if !Stricter(ModeDontAsk, ModeDefault) || Stricter(ModeAcceptEdits, ModeDefault) || Stricter(ModeDefault, ModeDefault) {
		t.Error("Stricter")
	}
}

// Shift-Tab cycles default, acceptEdits and dontAsk; bypassPermissions
// is reached only by its name (/mode, -mode, -no-policy), and the next
// mode after it is default (#145's review, M2).
func TestTheCycleLeavesBypassOut(t *testing.T) {
	for from, want := range map[string]string{
		ModeDefault: ModeAcceptEdits, ModeAcceptEdits: ModeDontAsk, ModeDontAsk: ModeDefault, ModeBypass: ModeDefault,
	} {
		if got := NextMode(from); got != want {
			t.Errorf("NextMode(%s) = %s, want %s", from, got, want)
		}
	}
}
