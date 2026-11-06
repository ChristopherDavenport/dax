package agent

// "Yes, always": a person's approval of a call that reaches beyond it,
// for the rest of the session (a grant set under the conversation's
// grant scope) or kept for the project (a rule in the user's grants
// file, loaded as its own source). Both rank below every other source
// (policy.RankGrant), so a grant answers only what asks for want of a
// rule.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentturn"
	workspace "github.com/ChristopherDavenport/agentworkspace"

	"github.com/ChristopherDavenport/dax/internal/private"
	"github.com/ChristopherDavenport/dax/policy"
)

// GrantKind is how far a person's approval reaches.
type GrantKind string

const (
	// GrantOnce approves the call alone.
	GrantOnce GrantKind = ""
	// GrantSession approves it and, for the rest of the session, the
	// calls the offered rule matches.
	GrantSession GrantKind = "session"
	// GrantSaved is GrantSession kept for the project, in the user's
	// grants file, for the sessions after this one too.
	GrantSaved GrantKind = "saved"
)

// GrantOffer is what dax offers beside a permission: Rule, the rule an
// approval "always" would grant ("" offers only once), and Saved,
// whether it can be kept for the project.
type GrantOffer struct {
	Rule  string
	Saved bool
}

// grants are the rules granted for the session, which each grant
// activates again as one set.
type grants struct {
	mu      sync.Mutex
	session []string
}

// GrantOffer is the grant dax offers for the pending call callID: a rule
// only when the policy asked about it for want of one (its deferred
// verdict names no rule: a grant never answers a rule's ask, dax-coding's
// secret paths among them), in the words of the tool's extension
// (extension.Extension.GrantRule), or the tool's bare name for a tool no
// extension's GrantRule speaks for. A call not pending, or one the
// engine did not defer in the run that left it, is offered nothing.
func (s *Session) GrantOffer(ctx context.Context, callID string) GrantOffer {
	if s.Kit == nil || s.Kit.Engine() == nil {
		return GrantOffer{}
	}
	st := s.ag.State()
	i := slices.IndexFunc(st.Pending, func(p agentturn.PendingCall) bool { return p.Call != nil && p.Call.CallID == callID })
	if i < 0 {
		return GrantOffer{}
	}
	call := st.Pending[i].Call
	v, ok := s.Kit.Engine().Deferred(st.RunID, callID)
	if !ok || v.Action != agentturn.Defer || v.Rule != nil {
		return GrantOffer{}
	}
	rule := call.Name
	if fn, ok := s.grantRules[call.Name]; ok {
		rule = fn(ctx, call.Name, json.RawMessage(call.Arguments))
	}
	if r, err := agentpolicy.ParseRules(rule); rule == "" || err != nil || len(r) != 1 {
		return GrantOffer{}
	}
	return GrantOffer{Rule: rule, Saved: s.opts.grantsFile() != ""}
}

// Grant makes the grant a person chose with their approval of the
// pending call callID: rule must be the one GrantOffer offers for it.
// GrantSession activates the session's grants as one set under the
// conversation's grant scope (agentpolicy's GrantSet, recorded with its
// scope), which a mode switch leaves in force and which ends with the
// conversation. GrantSaved keeps the rule in the user's grants file
// for the project and rebuilds the policy with it (SetPolicy, its
// reason the rule). GrantOnce does nothing. Make it before the Resume
// that answers the call, while the engine still holds its verdict.
func (s *Session) Grant(ctx context.Context, callID string, kind GrantKind, rule string) error {
	if kind == GrantOnce {
		return nil
	}
	offer := s.GrantOffer(ctx, callID)
	switch {
	case offer.Rule == "" || offer.Rule != rule:
		return fmt.Errorf("dax offers no grant of %s for this call", rule)
	case kind == GrantSaved && !offer.Saved:
		return errors.New("this session keeps no grants for the project")
	}
	eng := s.Kit.Engine()
	switch kind {
	case GrantSession:
		s.grants.mu.Lock()
		defer s.grants.mu.Unlock()
		rules := s.grants.session
		if !slices.Contains(rules, rule) {
			rules = append(slices.Clone(rules), rule)
		}
		parsed, err := agentpolicy.ParseRules(strings.Join(rules, " "))
		if err != nil {
			return err
		}
		sctx := agentpolicy.ContextWithGrantScope(ctx, s.Kit.GrantScope(ctx))
		_, refused := eng.GrantSet(sctx, agentpolicy.RuleSet{
			Source: agentpolicy.Source{Name: policy.SourceSession, Trusted: true, Rank: policy.RankGrant},
			Allow:  parsed,
		})
		for _, r := range refused {
			if ruleString(r.Rule) == rule {
				return fmt.Errorf("%s was not granted: %s", rule, r.Reason)
			}
		}
		s.grants.session = rules
		return nil
	case GrantSaved:
		if err := saveGrant(s.opts.grantsFile(), s.ws.Descriptor(), rule); err != nil {
			return fmt.Errorf("%s was not kept: %w", rule, err)
		}
		s.modes.mu.Lock()
		defer s.modes.mu.Unlock()
		st := s.modes.settings
		if st == nil {
			return ErrNoPolicy
		}
		if !slices.Contains(st.Saved, rule) {
			st.Saved = append(slices.Clone(st.Saved), rule)
		}
		p, err := policy.Build(*st)
		if err != nil {
			return err
		}
		return eng.SetPolicy(ctx, p, "kept "+rule+" for the project")
	}
	return fmt.Errorf("grant %q: want session or saved", kind)
}

// ruleString is a rule as it is written.
func ruleString(r agentpolicy.Rule) string {
	if r.Spec == "" {
		return r.Tool
	}
	return r.Tool + "(" + r.Spec + ")"
}

// DefaultGrantsFile is where the rules the user kept for each project
// are: grants.json in dir, the user's dax directory.
func DefaultGrantsFile(dir string) string { return filepath.Join(dir, "grants.json") }

// grantsFile is where o's saved grants are, "" for none: a session with
// no user directory (-bare) keeps none.
func (o Options) grantsFile() string {
	if o.UserDir == "" || o.Bare != nil {
		return ""
	}
	return DefaultGrantsFile(o.UserDir)
}

// grantsFileData is the grants file: per project, by its workspace (its
// identity, as -list has it), the allow rules kept for it.
type grantsFileData struct {
	Projects []savedGrants `json:"projects"`
}

type savedGrants struct {
	Workspace recordedWorkspace `json:"workspace"`
	Allow     []string          `json:"allow"`
}

// readGrants is the rules file keeps for the project in d; a missing
// file keeps none, and so does one another account could have written,
// with an error that says so. A rule that does not parse is left out.
func readGrants(file string, d workspace.Descriptor) ([]string, error) {
	g, err := readGrantsFile(file)
	if err != nil {
		return nil, err
	}
	key := recordedWorkspace{Kind: d.Kind, Ref: d.Ref, Root: d.Root}
	var out []string
	for _, p := range g.Projects {
		if p.Workspace != key {
			continue
		}
		for _, r := range p.Allow {
			if parsed, err := agentpolicy.ParseRules(r); err == nil && len(parsed) == 1 && !strings.HasPrefix(parsed[0].Spec, "!") {
				out = append(out, r)
			}
		}
	}
	return out, nil
}

func readGrantsFile(file string) (grantsFileData, error) {
	var g grantsFileData
	if err := checkPrivate(file); err != nil {
		return g, fmt.Errorf("the grants file %s: %w; keeping no grants from it", file, err)
	}
	b, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return g, nil
	}
	if err != nil {
		return g, fmt.Errorf("the grants file: %w", err)
	}
	if err := json.Unmarshal(b, &g); err != nil {
		return grantsFileData{}, fmt.Errorf("the grants file %s: %w; keeping no grants from it", file, err)
	}
	return g, nil
}

// saveGrant adds rule to what file keeps for the project in d, in a
// private directory, the file replaced whole.
func saveGrant(file string, d workspace.Descriptor, rule string) error {
	if err := private.SecureDir(filepath.Dir(file)); err != nil {
		return err
	}
	g, err := readGrantsFile(file)
	if err != nil {
		return err
	}
	key := recordedWorkspace{Kind: d.Kind, Ref: d.Ref, Root: d.Root}
	i := slices.IndexFunc(g.Projects, func(p savedGrants) bool { return p.Workspace == key })
	if i < 0 {
		g.Projects = append(g.Projects, savedGrants{Workspace: key})
		i = len(g.Projects) - 1
	}
	if !slices.Contains(g.Projects[i].Allow, rule) {
		g.Projects[i].Allow = append(g.Projects[i].Allow, rule)
	}
	b, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".grants-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), file)
}
