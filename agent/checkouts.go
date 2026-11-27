package agent

// The git checkouts a session works in (agentsession format 0.22, the
// env entry's checkouts; agentsession docs/plans/checkouts.md). The
// session follows the checkouts its tools act in: the one at the
// workspace's root, any a git command runs in, and any a git command
// makes, a worktree say; never every worktree of a repository. It
// reads each through the workspace (tool.ReadCheckout) at a run's
// start and end, and the one a git command ran in after the call
// (Env.CheckoutsMoved, which dax-coding calls after a bash call that
// runs git), and the recorder writes an env entry when what it read
// differs from the one in force (agentturn's WithEnv and Recorder.Env).

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"

	"github.com/ChristopherDavenport/agentsession"

	"github.com/ChristopherDavenport/dax/tool"
)

// checkouts is the set a session follows and what it last read of
// each.
type checkouts struct {
	mu sync.Mutex
	// seeded is set once the set has taken up the checkouts of the env
	// in force on a resumed path.
	seeded bool
	// noGit is set once a read found no git in the workspace: nothing
	// there can move a checkout, so the session stops reading, and the
	// env entry says nothing of checkouts.
	noGit bool
	// order are the checkouts' paths as the env entry writes them, in
	// the order the session first followed them; state what was last
	// read of each, and dir the directory, relative to the workspace's
	// root, a read of it runs in.
	order []string
	state map[string]agentsession.Checkout
	dir   map[string]string
	// root is the path of the checkout holding the workspace's root,
	// empty when the root is in none: its vcs is the entry's own.
	root string
	// pending are the directories the next read looks at alone, set by
	// a git command's call; nil reads every checkout followed.
	pending []string
	// first is each checkout's revision when the session began to
	// follow it: what a run's snapshot of a checkout it made is against.
	first map[string]string
	// said are the problems already reported, so each is said once;
	// saidMu guards it apart from mu, since a report is made with mu
	// held and without.
	saidMu sync.Mutex
	said   map[string]bool
}

// moved marks dirs (relative to the workspace's root) as the ones a
// call's git commands ran in or made, for the next read to look at;
// none marks every checkout followed.
func (c *checkouts) moved(dirs []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(dirs) == 0 {
		c.pending = nil
		return
	}
	c.pending = append([]string{}, dirs...)
}

// checkoutSay reports a problem once a session.
func (s *Session) checkoutSay(key, format string, args ...any) {
	c := &s.checkouts
	c.saidMu.Lock()
	defer c.saidMu.Unlock()
	if c.said == nil {
		c.said = map[string]bool{}
	}
	if c.said[key] {
		return
	}
	c.said[key] = true
	s.opts.log(format, args...)
}

// readCheckouts reads the checkouts the next env entry lists, the ones
// a call marked or every one followed, and returns them with the vcs of
// the checkout holding the workspace's root. ok is false when the
// session records no checkouts: it is not recorded, or the workspace
// has no git.
func (s *Session) readCheckouts(ctx context.Context) (list []agentsession.Checkout, rootVCS *agentsession.VCS, rooted, ok bool) {
	if !s.recorded || s.files == nil {
		return nil, nil, false, false
	}
	c := &s.checkouts
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.noGit {
		return nil, nil, false, false
	}
	if c.state == nil {
		c.state, c.dir, c.first = map[string]agentsession.Checkout{}, map[string]string{}, map[string]string{}
	}
	s.seedCheckouts()
	dirs := c.pending
	c.pending = nil
	if dirs == nil {
		// Every checkout followed, and the root, which may have become
		// one (git init) or stopped being one.
		dirs = []string{"."}
		for _, p := range c.order {
			if d := c.dir[p]; d != "." {
				dirs = append(dirs, d)
			}
		}
	}
	for _, d := range dirs {
		if !s.readCheckout(ctx, d) {
			return nil, nil, false, false
		}
	}
	list = make([]agentsession.Checkout, 0, len(c.order))
	for _, p := range c.order {
		list = append(list, cloneCheckout(c.state[p]))
	}
	if c.root != "" {
		rc := c.state[c.root]
		return list, cloneVCS(rc.VCS), true, true
	}
	return list, nil, false, true
}

// seedCheckouts takes up, once, the checkouts the env in force on the
// session's path lists, so a resumed session goes on following what it
// followed. Only those a read can reach again are kept: a path in the
// workspace. c.mu is held.
func (s *Session) seedCheckouts() {
	c := &s.checkouts
	if c.seeded || s.Kit == nil {
		return
	}
	live := s.Kit.Session()
	if live == nil {
		return
	}
	c.seeded = true
	env := agentsession.EnvInForce(live.Path(live.Leaf()))
	if env == nil {
		return
	}
	for _, co := range env.Checkouts {
		if co.Path == "." || filepath.IsAbs(filepath.FromSlash(co.Path)) || !agentsession.ValidCheckoutPath(co.Path) {
			continue
		}
		if _, had := c.state[co.Path]; !had {
			c.order = append(c.order, co.Path)
			c.state[co.Path] = cloneCheckout(co)
			c.dir[co.Path] = co.Path
			c.first[co.Path] = revisionOf(co)
		}
	}
}

// readCheckout reads the checkout holding dir and records it, following
// it from now on; a dir in no checkout, or one gone, stops following
// whatever was followed there. It returns false once the workspace is
// found to have no git. c.mu is held.
func (s *Session) readCheckout(ctx context.Context, dir string) bool {
	c := &s.checkouts
	g, err := tool.ReadCheckout(ctx, s.files, dir)
	switch {
	case errors.Is(err, tool.ErrNoGit):
		c.noGit = true
		s.checkoutSay("nogit", "dax: git is not installed in the workspace, so the record follows no checkouts")
		return false
	case errors.Is(err, tool.ErrGone):
		s.dropCheckoutAt(dir)
		return true
	case errors.Is(err, tool.ErrOutside):
		return true
	case err != nil && !g.Repo:
		s.checkoutSay("read:"+dir, "dax: could not read the checkout at %s: %v", dir, err)
		return true
	case !g.Repo:
		s.dropCheckoutAt(dir)
		if dir == "." {
			c.root = ""
		}
		return true
	}
	path := s.checkoutPath(g.Top)
	if path == "" {
		return true
	}
	co := agentsession.Checkout{Path: path, Repo: s.checkoutPath(g.CommonDir)}
	switch {
	case err != nil:
		// Where it is was read, its state was not: listed with no vcs,
		// as a checkout the writer knows of but could not read.
		s.checkoutSay("read:"+path, "dax: could not read the checkout at %s: %v", path, err)
	case g.Withheld != "" || g.JJ:
	default:
		co.VCS = &agentsession.VCS{System: "git", Revision: g.Revision, Dirty: g.Dirty}
		co.Branch = g.Branch
		if g.Upstream != "" {
			co.Upstream = &agentsession.Upstream{Name: g.Upstream, Revision: g.UpstreamRevision}
		}
	}
	if _, had := c.state[path]; !had {
		c.order = append(c.order, path)
		c.first[path] = revisionOf(co)
	}
	c.state[path] = co
	if _, had := c.dir[path]; !had || dir == path {
		c.dir[path] = s.checkoutDir(path, dir)
	}
	if dir == "." {
		c.root = path
	}
	return true
}

// dropCheckoutAt stops following the checkout whose directory is dir.
// c.mu is held.
func (s *Session) dropCheckoutAt(dir string) {
	c := &s.checkouts
	for p, d := range c.dir {
		if d == dir || p == dir {
			delete(c.state, p)
			delete(c.dir, p)
			delete(c.first, p)
			c.order = slices.DeleteFunc(c.order, func(q string) bool { return q == p })
			if c.root == p {
				c.root = ""
			}
		}
	}
}

// checkoutPath is a directory git named, absolute in the workspace's
// namespace, as the env entry writes it: relative to the workspace's
// root when under it ("." for the root), absolute otherwise; empty for
// one the format cannot hold.
func (s *Session) checkoutPath(abs string) string {
	var p string
	if rel, err := s.files.Rel(abs); err == nil {
		p = filepath.ToSlash(rel)
	} else {
		p = filepath.ToSlash(filepath.Clean(abs))
	}
	if !agentsession.ValidCheckoutPath(p) {
		return ""
	}
	return p
}

// checkoutDir is where a read of the checkout at path runs: its top
// when that is in the workspace, else dir, the directory the read was
// asked for (the root, for a checkout holding it).
func (s *Session) checkoutDir(path, dir string) string {
	if !filepath.IsAbs(filepath.FromSlash(path)) {
		return path
	}
	return dir
}

// revisionOf is the commit co is on, empty when it was not read.
func revisionOf(co agentsession.Checkout) string {
	if co.VCS == nil {
		return ""
	}
	return co.VCS.Revision
}

// changedCheckout is a checkout a run changed, for its snapshot: where
// a read of it runs, and the commit its change is against.
type changedCheckout struct {
	path, dir, base string
}

// changedSince lists the checkouts followed, other than the one
// holding the workspace's root, whose commit differs from base's (or,
// for one base does not list, from the one it was first followed at)
// or whose tree is dirty, in the order they were first followed.
func (s *Session) changedSince(base map[string]string) []changedCheckout {
	c := &s.checkouts
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []changedCheckout
	for _, p := range c.order {
		co := c.state[p]
		if p == c.root || co.VCS == nil {
			continue
		}
		b, ok := base[p]
		if !ok {
			b = c.first[p]
		}
		if co.VCS.Revision != b || co.VCS.Dirty {
			out = append(out, changedCheckout{path: p, dir: c.dir[p], base: b})
		}
	}
	return out
}

// revisions are the commits of the checkouts followed now, by path.
func (s *Session) checkoutRevisions() map[string]string {
	c := &s.checkouts
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]string, len(c.order))
	for _, p := range c.order {
		out[p] = revisionOf(c.state[p])
	}
	return out
}

// cloneCheckout is a copy of co that shares nothing with it.
func cloneCheckout(co agentsession.Checkout) agentsession.Checkout {
	out := co
	out.VCS = cloneVCS(co.VCS)
	if co.Upstream != nil {
		u := *co.Upstream
		out.Upstream = &u
	}
	return out
}

// cloneVCS is a copy of v, nil for nil.
func cloneVCS(v *agentsession.VCS) *agentsession.VCS {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}

// CheckoutsMoved implements extension.Env: the checkouts holding dirs
// are read again, and followed from now on, and the recorder writes an
// env entry, in the session ctx's call belongs to, when one changed.
// No dirs reads every checkout followed. A read or a write that fails
// is reported once and never fails the call.
func (e *sessionEnv) CheckoutsMoved(ctx context.Context, dirs ...string) {
	s := e.s
	if !s.recorded || s.Kit == nil {
		return
	}
	rec := s.Kit.Recorder()
	if rec == nil {
		return
	}
	s.checkouts.moved(dirs)
	if err := rec.Env(ctx); err != nil {
		s.checkoutSay("env", "dax: recording the checkouts: %v", err)
	}
}

// checkoutsAtRunEnd reads every checkout followed once a run of the
// main agent has ended, so a commit a script made, which no call said
// was git, is on the record before the next run. A run whose snapshot
// StartSnapshot began has it read once EndSnapshot has written the
// snapshot, which targets the run's end and follows it.
func (s *Session) checkoutsAtRunEnd(ctx context.Context) {
	if !s.recorded || s.Kit == nil || s.snap.pending() {
		return
	}
	s.writeCheckouts(ctx)
}

// writeCheckouts reads every checkout followed and has the recorder
// write an env entry when one changed.
func (s *Session) writeCheckouts(ctx context.Context) {
	if !s.recorded || s.Kit == nil {
		return
	}
	rec := s.Kit.Recorder()
	if rec == nil {
		return
	}
	s.checkouts.moved(nil)
	if err := rec.Env(context.WithoutCancel(ctx)); err != nil {
		s.checkoutSay("env", "dax: recording the checkouts: %v", err)
	}
}
