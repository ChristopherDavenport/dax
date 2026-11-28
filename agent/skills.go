package agent

import (
	"context"
	"fmt"
	"regexp"

	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// Skill is a skill the person can invoke as /<Name>.
type Skill struct {
	Name        string
	Description string
	// Location is the SKILL.md behind the name.
	Location string
	// Untrusted says its text is not the person's own (a repository's,
	// or one the model wrote and the person has not approved): its
	// instructions are sent marked untrusted.
	Untrusted bool
}

// skillCommand is a skill name a slash command can carry: the Agent
// Skills name grammar. A skill listed under a qualified name, because
// another holds its own, is the model's to read alone.
var skillCommand = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Skills are the skills the session's person can invoke, in the
// catalogue's order: the ones the skill tool serves, by the names a
// command can carry. None without skills.
func (s *Session) Skills() []Skill {
	if s.Kit == nil {
		return nil
	}
	cat := s.Kit.Catalog()
	if cat == nil {
		return nil
	}
	var out []Skill
	for _, sk := range cat.Listed() {
		if name := sk.ListedName(); skillCommand.MatchString(name) {
			out = append(out, Skill{Name: name, Description: sk.Description, Location: sk.Location, Untrusted: !s.ownsSkill(sk)})
		}
	}
	return out
}

// ownsSkill is whether sk's text is the person's own, as the extension
// that gives the skills says (extension.Env.OwnSkills); no skill is
// when none says.
func (s *Session) ownsSkill(sk *agentskill.Skill) bool {
	return s.ownSkill != nil && s.ownSkill(sk)
}

// Skill is the message /<name> args sends: the skill read on the
// person's behalf (agentkit's Kit.SkillMessage), its instructions as
// the model's skill tool serves them and then args, marked as composed
// from the skill and, when its text is not the person's own, as
// untrusted. The turn that takes the message makes the read the model
// would have made: it grants what a read of the skill grants, under the
// session's skill trust, until the next message, and records the read
// as made for the person. The read asks nothing of the policy: the
// person typed the command, as with a prompt template. It is the
// request the person chose to send, so a fold keeps it verbatim.
func (s *Session) Skill(ctx context.Context, name, args string) (openresponses.Item, error) {
	if s.Kit == nil || !skillCommand.MatchString(name) {
		return nil, fmt.Errorf("no skill /%s", name)
	}
	cat := s.Kit.Catalog()
	if cat == nil {
		return nil, fmt.Errorf("no skill /%s", name)
	}
	sk, ok := cat.Lookup(name)
	if !ok || sk.ListedName() != name {
		return nil, fmt.Errorf("no skill /%s", name)
	}
	trust := &agentturn.Trust{}
	if !s.ownsSkill(sk) {
		trust.Reads = []agenttool.Label{agenttool.LabelUntrusted}
	}
	item, _, err := s.Kit.SkillMessage(ctx, name, args, trust)
	if err != nil {
		return nil, err
	}
	s.tasks.add([]openresponses.Item{item})
	return item, nil
}
