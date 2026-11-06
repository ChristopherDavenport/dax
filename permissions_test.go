package dax

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/policy"
)

// /permissions lists the rules in force by source, each source with the
// file it came from, the default, the mode and the grant sets with
// their scope; dax attach shows the same lines from the served info.
func TestPermissionsListsTheRulesAndWhereTheyCameFrom(t *testing.T) {
	m := &steps{parent: [][2]string{{"bash", `{"command":"echo hello"}`}}}
	userPath := filepath.Join(t.TempDir(), "config.json")
	r := startRig(t, m, policy.Rules{Allow: []string{"bash(git push:*)"}}, func(o *agent.Options) {
		o.Policy.UserPath = userPath
	}, nil, false)
	ctx := context.Background()
	res, err := command(ctx, r.sess, r.sess, "/permissions", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"mode default",
		"default: ask",
		"dax:config (" + userPath + ")",
		"  allow bash(git push:*)",
		"extension:dax-coding",
		"  ask read(ci:.env*)",
	} {
		if !strings.Contains(res.out, want) {
			t.Errorf("/permissions lacks %q:\n%s", want, res.out)
		}
	}

	// A session grant lists with its scope.
	if _, err := agent.Drive(ctx, r.sess, agent.Rules{Permit: func(c *openresponses.FunctionCall, _ string) agent.PermitAnswer {
		o := r.sess.GrantOffer(ctx, c.CallID)
		return agent.PermitAnswer{Allow: true, Grant: agent.GrantSession, Rule: o.Rule}
	}}, openresponses.UserText("go")); err != nil {
		t.Fatal(err)
	}
	res, err = command(ctx, r.sess, r.sess, "/permissions", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.out, "granted dax:session (scope "+r.sess.ID()+")") || !strings.Contains(res.out, "  allow bash(echo hello)") {
		t.Errorf("the session's grant is not listed:\n%s", res.out)
	}

	// dax attach reads the same from the served info.
	raw, err := serveCommands(r.sess, r.sess, "")[cmdInfo].Run(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var si servedInfo
	if err := json.Unmarshal(raw, &si); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(permissionLines(si.Info), "\n"); got != res.out {
		t.Errorf("attach shows:\n%s\nwant:\n%s", got, res.out)
	}
}
