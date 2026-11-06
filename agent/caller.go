package agent

// What an A2A caller's requests leave out of what the person's carry:
// the person's own AGENTS.md (UserDir/AGENTS.md and AgentsMDGlobal),
// which the kit renders in the one AGENTS.md part every conversation
// shares, and the definitions of the tools the extensions keep to the
// person (extension.Extension.PersonOnly: the memory tools,
// skill_manage), which personOnlyRefusal refuses a caller anyway. A
// caller's request, its own and its sub-agents', is the kit's with
// that part rendered again without those files and those tools taken
// out, after the kit's own BeforeModelCall, so the caller's record
// holds what it was sent.

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentsmd"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/openresponses"
)

// callerMD is the AGENTS.md part as the person's requests carry it and
// as a caller's do, and the person's own files' tags, which no caller's
// request may hold.
type callerMD struct {
	all, caller string
	tags        []string
}

// newCallerMD is files' part for the person and for a caller, the
// files at personal left out of the caller's.
func newCallerMD(files []agentsmd.File, personal []string) callerMD {
	c := callerMD{all: agentsmd.Render(files)}
	var kept []agentsmd.File
	for _, f := range files {
		if mine(personal, f.Path) {
			// The tag the file is rendered under: its path, escaped as
			// Render escapes it.
			tag := agentsmd.Render([]agentsmd.File{{Path: f.Path}})
			if i := strings.Index(tag, "<project_instructions"); i >= 0 {
				tag = tag[i:]
				if j := strings.Index(tag, ">"); j >= 0 {
					tag = tag[:j+1]
				}
			}
			c.tags = append(c.tags, tag)
			continue
		}
		kept = append(kept, f)
	}
	c.caller = agentsmd.Render(kept)
	return c
}

func mine(personal []string, p string) bool {
	for _, q := range personal {
		if filepath.Clean(q) == filepath.Clean(p) {
			return true
		}
	}
	return false
}

// errCallerMD fails a caller's request whose instructions still name a
// file of the person's own after the part was rendered again: a guard
// or a hook rewrote the part, and the request is not sent rather than
// sent with it.
var errCallerMD = errors.New("the person's own AGENTS.md could not be left out of an A2A caller's request")

// instructions is in with the AGENTS.md part as a caller's request
// carries it.
func (c callerMD) instructions(in string) (string, error) {
	if len(c.tags) == 0 {
		return in, nil
	}
	switch {
	case c.caller != "":
		in = strings.Replace(in, c.all, c.caller, 1)
	case strings.Contains(in, agentkit.Separator+c.all):
		in = strings.Replace(in, agentkit.Separator+c.all, "", 1)
	case strings.Contains(in, c.all+agentkit.Separator):
		in = strings.Replace(in, c.all+agentkit.Separator, "", 1)
	default:
		in = strings.Replace(in, c.all, "", 1)
	}
	for _, t := range c.tags {
		if strings.Contains(in, t) {
			return "", errCallerMD
		}
	}
	return in, nil
}

// parts are ps with the AGENTS.md part as a caller's request carries
// it, left out when nothing of it is left.
func (c callerMD) parts(ps []agentkit.Part) []agentkit.Part {
	if len(c.tags) == 0 {
		return ps
	}
	var out []agentkit.Part
	for _, p := range ps {
		if p.ID == agentsmd.PartID {
			if c.caller == "" {
				continue
			}
			p.Text = c.caller
		}
		out = append(out, p)
	}
	return out
}

// callerRequest is the BeforeModelCall, after the kit's own, of the
// main agent's runs and the sub-agents' (extension.Env's
// ChildBeforeModelCall): on an A2A caller's context, the request with
// the person's own AGENTS.md and the tools they keep to themselves
// left out; any other request as it is.
func (s *Session) callerRequest(ctx context.Context, req *openresponses.Request) error {
	if _, peer := callerOf(ctx); !peer {
		return nil
	}
	in, err := s.callerMD.instructions(req.Instructions)
	if err != nil {
		return err
	}
	req.Instructions = in
	if slices.ContainsFunc(req.Tools, s.isPersonOnly) {
		req.Tools = slices.DeleteFunc(slices.Clone(req.Tools), s.isPersonOnly)
	}
	return nil
}

// isPersonOnly reports whether t is the definition of a tool an
// extension keeps to the person.
func (s *Session) isPersonOnly(t openresponses.Tool) bool {
	f, ok := t.(*openresponses.FunctionTool)
	return ok && s.personOnly[f.Name]
}

// offeredToCallers is ts without the tools an extension keeps to the
// person: what a caller's run (PeerConfig) is offered and its agent
// card names.
func (s *Session) offeredToCallers(ts []agenttool.Tool) []agenttool.Tool {
	if !slices.ContainsFunc(ts, func(t agenttool.Tool) bool { return s.personOnly[t.Name()] }) {
		return ts
	}
	return slices.DeleteFunc(slices.Clone(ts), func(t agenttool.Tool) bool { return s.personOnly[t.Name()] })
}
