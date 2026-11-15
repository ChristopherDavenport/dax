package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/policy"
)

// A grant for the session is made again when the session is resumed
// (#186): the call it covers runs unasked, a call outside it still
// asks; a grant revoked before the resume stays revoked.
func TestASessionGrantComesBackOnResume(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		t.Run(map[bool]string{false: "granted", true: "revoked"}[revoked], func(t *testing.T) {
			ctx := context.Background()
			model := perPrompt{
				"go":    {{"bash", `{"command":"echo hello"}`}},
				"again": {{"bash", `{"command":"echo hello"}`}},
				"other": {{"bash", `{"command":"echo bye"}`}},
			}
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
			if revoked {
				sctx := agentpolicy.ContextWithGrantScope(ctx, s.Kit.GrantScope(ctx))
				s.Kit.Engine().Revoke(sctx, policy.SourceSession)
			}
			id := s.ID()
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}

			r, err := Resume(ctx, o, id)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			asked = nil
			if _, err := Drive(ctx, r, grantPermit(r, GrantOnce, &asked, &offered), openresponses.UserText("again")); err != nil {
				t.Fatal(err)
			}
			if want := map[bool]int{false: 0, true: 1}[revoked]; len(asked) != want {
				t.Errorf("the resumed session asked about %v, want %d", asked, want)
			}
			asked = nil
			if _, err := Drive(ctx, r, grantPermit(r, GrantOnce, &asked, &offered), openresponses.UserText("other")); err != nil {
				t.Fatal(err)
			}
			if len(asked) != 1 {
				t.Errorf("a call outside the grant asked %v, want once", asked)
			}
		})
	}
}
