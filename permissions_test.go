package dax

import (
	"context"
	"encoding/json"
	"os"
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

// /skills approve NAME approves a skill the model wrote, as it is now,
// and names its allowed-tools; a name that is no learned skill is
// refused (#159).
func TestSkillsApprove(t *testing.T) {
	r := startRig(t, &steps{}, policy.Rules{}, nil, nil, false)
	ctx := context.Background()
	dir := filepath.Join(r.o.UserDir, "learned", "fix-migration")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: fix-migration\ndescription: Rerun it.\nallowed-tools: Bash(echo:*)\n---\nRerun.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := command(ctx, r.sess, r.sess, "/skills approve fix-migration", false)
	if err != nil || !strings.Contains(res.out, "approved the skill fix-migration") || !strings.Contains(res.out, "Bash(echo:*)") {
		t.Fatalf("/skills approve: %q, %v", res.out, err)
	}
	if data, err := os.ReadFile(filepath.Join(r.o.UserDir, "skill-approvals.json")); err != nil || !strings.Contains(string(data), `"fix-migration": "`) {
		t.Errorf("approvals file %q, %v", data, err)
	}
	for _, line := range []string{"/skills approve greet", "/skills approve ../learned/fix-migration", "/skills", "/skills list"} {
		if _, err := command(ctx, r.sess, r.sess, line, false); err == nil {
			t.Errorf("%s was accepted", line)
		}
	}
}
