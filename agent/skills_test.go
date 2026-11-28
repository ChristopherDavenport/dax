package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// A skill the person invokes is read for them: its message carries the
// instructions and the arguments, the call the skill allows runs
// unasked under -trust-skills, the record says the read was made for
// the person, and the next message ends the grant (#230).
func TestASkillThePersonInvokesIsReadForThem(t *testing.T) {
	ctx := context.Background()
	model := &scripted{calls: [][2]string{{"bash", `{"command":"echo hello"}`}}}
	o := trustSkills(options(t, model))
	o.Policy = confirmPolicy(t)
	var asked []string
	approve := func(c *openresponses.FunctionCall, _ string) bool {
		asked = append(asked, c.Arguments)
		return true
	}
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	list := s.Skills()
	if len(list) != 1 || list[0].Name != "greet" || list[0].Untrusted || list[0].Description != "How to greet the user." {
		t.Fatalf("skills = %+v, want the user's greet, their own text", list)
	}
	item, err := s.Skill(ctx, "greet", "in French")
	if err != nil {
		t.Fatal(err)
	}
	base, _ := agentturn.Unhide(item)
	msg, ok := base.(*openresponses.Message)
	if !ok || len(msg.Content) != 3 {
		t.Fatalf("the message = %#v, want a lead, the instructions and the arguments", base)
	}
	if text := msg.Content[1].(*openresponses.InputText).Text; !strings.Contains(text, "Say hello.") {
		t.Errorf("the instructions part = %q", text)
	}
	if text := msg.Content[2].(*openresponses.InputText).Text; text != "in French" {
		t.Errorf("the arguments part = %q", text)
	}
	if _, err := Drive(ctx, s, rulesOf(approve), item); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 0 {
		t.Fatalf("asked about %v after the invoked skill granted Bash(echo:*)", asked)
	}
	// The next message ends the grant.
	if _, err := promptOn(ctx, s, "again", approve); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 {
		t.Fatalf("asked %v on the next message, want one question", asked)
	}
	data := string(projected(t, o, s))
	if !strings.Contains(data, `"by":"user"`) || !strings.Contains(data, `"source":"agentskill"`) {
		t.Errorf("the record lacks the read made for the person, or the message's layer:\n%s", data)
	}
	if _, err := s.Skill(ctx, "nope", ""); err == nil {
		t.Error("a skill that is not there was invoked")
	}
}

// A project's skill is the repository's text: listed as untrusted, sent
// marked so, and it grants nothing even under -trust-skills.
func TestAProjectsSkillInvokedIsUntrustedAndGrantsNothing(t *testing.T) {
	ctx := context.Background()
	model := &scripted{calls: [][2]string{{"bash", `{"command":"echo hello"}`}}}
	o := trustSkills(options(t, model))
	o.Policy = confirmPolicy(t)
	write(t, filepath.Join(o.Dir, ".dax", "skills", "deploy", "SKILL.md"),
		"---\nname: deploy\ndescription: How to deploy.\nallowed-tools: Bash(echo:*)\n---\nShip it.\n")
	var asked []string
	approve := func(c *openresponses.FunctionCall, _ string) bool {
		asked = append(asked, c.Arguments)
		return true
	}
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var deploy *Skill
	for _, sk := range s.Skills() {
		if sk.Name == "deploy" {
			deploy = &sk
		}
	}
	if deploy == nil || !deploy.Untrusted {
		t.Fatalf("skills = %+v, want the project's deploy, untrusted", s.Skills())
	}
	item, err := s.Skill(ctx, "deploy", "")
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := agentturn.ProvenanceOf(item); !ok || p.Trust == nil || len(p.Trust.Reads) != 1 || p.Trust.Reads[0] != agenttool.LabelUntrusted {
		t.Errorf("the message's provenance = %+v, want untrusted", p)
	}
	if _, err := Drive(ctx, s, rulesOf(approve), item); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 {
		t.Fatalf("asked %v, want the one question: a project's skill grants nothing", asked)
	}
}
