package agent

// This file is box.confine: which of a box's tools the session's policy
// takes as confined, so a bare ask rule for them does not ask (see
// docs/sandbox.md, "Fewer questions in a box"). The claim is the
// session's, made from what the box host said of each unit's envelope
// and from the user's own opt-in, never from what the executor or a
// unit says of itself.
//
// It works through agentpolicy's confinement: the session adds a bare
// ask rule of source policy.SourceBox for each tool the box may
// confine, and a call the function below claims skips it. An ask beats
// an allow whatever the source, so the function also claims every call
// the policy would allow without those rules (the baseline engine): no
// call that runs unasked today asks because the box's rules are there.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/internal/box"
	"github.com/ChristopherDavenport/dax/internal/config"
	"github.com/ChristopherDavenport/dax/internal/executor"
	"github.com/ChristopherDavenport/dax/policy"
	"github.com/ChristopherDavenport/dax/tool"
)

// codingExtension names the extension whose bash, write and edit the
// workspace's confinement may cover.
const codingExtension = "dax-coding"

// workspaceTools are the workspace's tools box.confine "workspace"
// covers: the ones that change the project, which is what asks today.
var workspaceTools = []string{"bash", "write", "edit"}

// boxEnvelope is one unit as the box host resolved it, the part of the
// handshake's box the claim reads (internal/box's UnitInfo).
type boxEnvelope struct {
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`
	Runtime  string   `json:"runtime"`
	Files    string   `json:"files"`
	Internet string   `json:"internet"`
	Secrets  []string `json:"secrets"`
	Guard    string   `json:"guard"`
}

// boxEnvelopes are the units raw, a box host's resolved box, names.
func boxEnvelopes(raw json.RawMessage) ([]boxEnvelope, error) {
	var info struct {
		Units []boxEnvelope `json:"units"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, fmt.Errorf("the box host's units: %w", err)
	}
	return info.Units, nil
}

// sandboxed reports whether runtime runs a container's processes on
// a kernel of its own (gVisor's, a Kata VM's), not this machine's as
// runc does, under which a container is a much weaker boundary.
func sandboxed(runtime string) bool {
	switch runtime {
	case "", "runc", "io.containerd.runc.v2":
		return false
	}
	return true
}

// containedFiles reports whether a unit's files keep it off the user's
// own: none, the project read-only, and its own state directory.
func containedFiles(files string) bool {
	if files == "none" {
		return true
	}
	if files == "" {
		return false
	}
	for _, f := range strings.Split(files, ",") {
		switch strings.TrimSpace(f) {
		case "project ro", "state rw":
		default:
			return false
		}
	}
	return true
}

// confiner is the session's claim of confinement for its box's tools.
type confiner struct {
	set *executor.Set
	// workspace is the label of the workspace's confinement, "" when its
	// tools are not claimed.
	workspace string
	// units are the claimed MCP units' labels by name.
	units map[string]string
	// baseline is the session's policy without SourceBox's rules: a call
	// it allows is claimed, so those rules never make it ask.
	baseline *agentpolicy.Engine
}

// childDecisionKey marks a sub-agent's decision, which box.confine
// does not cover: the explore agent's read-only guarantee rests on the
// analyzer, so a sub-agent's calls ask or run as they do without it.
type childDecisionKey struct{}

func childDecision(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, childDecisionKey{}, name)
}

// newConfiner is the claim box.confine mode makes over the box host's
// resolved box raw, nil when it claims nothing: mode off, no box, or no
// tool whose envelope qualifies. project are the project's ask rules: a
// tool a bare one of them covers is not claimed, since a repository's
// bare ask would otherwise turn into an allow. offered are the servers
// the box host offered and the session connected: a unit's tools are
// claimed only when it is one of them.
func newConfiner(mode string, raw json.RawMessage, set *executor.Set, project []string, aliases map[string][]string, offered []string) (*confiner, error) {
	if mode != config.ConfineUnits && mode != config.ConfineWorkspace || len(raw) == 0 || set == nil {
		return nil, nil
	}
	envs, err := boxEnvelopes(raw)
	if err != nil {
		return nil, err
	}
	asked, err := projectBareAsks(project, aliases)
	if err != nil {
		return nil, err
	}
	c := &confiner{set: set, units: map[string]string{}}
	for _, e := range envs {
		label := "box:" + e.Runtime + ":" + e.Name
		switch e.Kind {
		case "workspace":
			if mode != config.ConfineWorkspace || e.Internet != "off" || !sandboxed(e.Runtime) {
				break
			}
			// Only behind the git guard: a program a confined call runs
			// could otherwise write a hook or config the user's git runs.
			if e.Guard != box.GuardOn {
				why := e.Guard
				if why == "" {
					why = "the box host did not say it holds the repository's .git read-only (an older dax there?)"
				}
				fmt.Fprintf(warnings{}, "dax: box.confine workspace: the workspace's tools ask as without it: %s\n", why)
				break
			}
			c.workspace = label
		case "mcp":
			// A unit's tools are claimed only when the unit is a server
			// the box host offered and the session connected, besides an
			// envelope: never a name some other server took.
			if e.Internet == "off" && len(e.Secrets) == 0 && containedFiles(e.Files) && sandboxed(e.Runtime) && !asked.unit(e.Name) && slices.Contains(offered, e.Name) {
				c.units[e.Name] = label
			}
		}
	}
	if c.workspace != "" {
		for _, t := range workspaceTools {
			if asked.tool(t) {
				// One bare ask the project made covers the lot: claiming
				// the others would split what the repository asked for.
				c.workspace = ""
				break
			}
		}
	}
	if c.workspace == "" && len(c.units) == 0 {
		return nil, nil
	}
	return c, nil
}

// rules are the SourceBox rules c needs: a bare ask for each tool it
// may claim, which its claim skips.
func (c *confiner) rules() []string {
	var out []string
	if c.workspace != "" {
		out = append(out, workspaceTools...)
	}
	units := make([]string, 0, len(c.units))
	for u := range c.units {
		units = append(units, u)
	}
	slices.Sort(units)
	for _, u := range units {
		out = append(out, "mcp__"+u+"__*")
	}
	return out
}

// claim is agentpolicy's WithConfinement function: whether a call runs
// confined, and by what.
func (c *confiner) claim(ctx context.Context, t agenttool.Tool, args json.RawMessage) (bool, string) {
	if t == nil {
		return false, ""
	}
	name := t.Name()
	label := c.label(ctx, name, args)
	if ok, by := c.baselineAllows(ctx, t, args); ok {
		if label != "" {
			return true, label
		}
		return true, by
	}
	if label == "" {
		return agenttool.ConfinedBy(ctx, t, args)
	}
	return true, label
}

// label is what confines a call of name with args by c's own reading,
// "" for none: a claimed unit's tool, or, outside a sub-agent, a
// workspace tool whose call stays off the protected paths.
func (c *confiner) label(ctx context.Context, name string, args json.RawMessage) string {
	if _, child := ctx.Value(childDecisionKey{}).(string); child {
		return ""
	}
	for u, l := range c.units {
		if strings.HasPrefix(name, "mcp__"+u+"__") {
			return l
		}
	}
	if c.workspace == "" {
		return ""
	}
	b, ok := c.set.ByName(name)
	if !ok || b.Extension != codingExtension {
		return ""
	}
	switch name {
	case "bash", "write", "edit":
	default:
		return ""
	}
	f, err := c.set.Facts(ctx, name, args)
	if err != nil {
		return ""
	}
	if name == "bash" {
		if !tool.ConfinableBash(args, f) {
			return ""
		}
	} else if !tool.ConfinablePath(f) {
		return ""
	}
	return c.workspace
}

// baselineAllows reports whether the policy without SourceBox's rules
// allows the call, and a label that says so for the verdict, which
// names the box's rule it skipped.
func (c *confiner) baselineAllows(ctx context.Context, t agenttool.Tool, args json.RawMessage) (bool, string) {
	if c.baseline == nil {
		return false, ""
	}
	v, err := c.baseline.Would(ctx, agentturn.ToolCallInfo{
		Call: &openresponses.FunctionCall{Name: t.Name(), Arguments: string(args)},
		Tool: t, Args: args,
	})
	if err != nil || v.Action != agentturn.Allow {
		return false, ""
	}
	by := "the policy without the box's rules"
	if v.Rule != nil {
		by = "allowed by " + v.Rule.String()
	}
	return true, by
}

// bareAsks are the tools and units a project's bare ask rules cover.
type bareAsks struct{ rules []agentpolicy.Rule }

// projectBareAsks are project's bare ask rules, aliases expanded.
func projectBareAsks(project []string, aliases map[string][]string) (bareAsks, error) {
	rules, err := agentpolicy.ParseRules(strings.Join(project, " "))
	if err != nil {
		return bareAsks{}, fmt.Errorf("policy ask: %w", err)
	}
	var out bareAsks
	for _, r := range rules {
		if !r.Bare() {
			continue
		}
		if names, ok := aliases[r.Tool]; ok {
			for _, n := range names {
				out.rules = append(out.rules, agentpolicy.Rule{Tool: n})
			}
			continue
		}
		out.rules = append(out.rules, r)
	}
	return out, nil
}

func (a bareAsks) tool(name string) bool {
	for _, r := range a.rules {
		if r.MatchesTool(name) {
			return true
		}
	}
	return false
}

// unit reports whether a bare ask covers any tool of unit u: one that
// names its prefix, or a glob whose fixed start a tool of it can have.
func (a bareAsks) unit(u string) bool {
	prefix := "mcp__" + u + "__"
	for _, r := range a.rules {
		if !r.Glob() {
			if strings.HasPrefix(r.Tool, prefix) {
				return true
			}
			continue
		}
		lit, _, _ := strings.Cut(r.Tool, "*")
		if strings.HasPrefix(prefix, lit) || strings.HasPrefix(lit, prefix) {
			return true
		}
	}
	return false
}

// confineBox builds the session's claim over settings, the policy
// settings the session runs under, and adds its SourceBox rules to
// them: the baseline is the policy as it was. It returns nil options
// when nothing is claimed.
func confineBox(o Options, set *executor.Set, settings *policy.Settings, matchers map[string]agentpolicy.ToolMatcher, aliases map[string][]string, offered []string) ([]agentpolicy.Option, error) {
	if o.Executor == nil {
		return nil, nil
	}
	c, err := newConfiner(o.BoxConfine, o.Executor.r.Box(), set, settings.Project.Ask, aliases, offered)
	if err != nil || c == nil {
		return nil, err
	}
	p0, err := policy.Build(*settings)
	if err != nil {
		return nil, err
	}
	if c.baseline, err = agentpolicy.Build(p0, matchers, agentpolicy.WithAliases(aliases), agentpolicy.WithConfinement(nil)); err != nil {
		return nil, fmt.Errorf("box.confine: %w", err)
	}
	settings.Box = c.rules()
	return []agentpolicy.Option{agentpolicy.WithConfinement(c.claim)}, nil
}
