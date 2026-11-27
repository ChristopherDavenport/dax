package agent

// The workspace a run left (#191): at a run's start the session reads
// the commit the workspace's git tree is on, and its change against it
// when the tree is dirty; at its end, the change again. The commit is
// the env entry's vcs; the changes are agenteval's snapshot entries
// (agenteval.NewSnapshot), so that a run dax recorded is read back by
// agenteval.ResultAt as a runner's would be. Both are read through the
// workspace the tools act in (tool.GitDiff), whichever machine it is
// on.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agenteval"
	"github.com/ChristopherDavenport/agentsession"

	"github.com/ChristopherDavenport/dax/internal/config"
	"github.com/ChristopherDavenport/dax/tool"
)

// DefaultSnapshotMax is the most bytes of a snapshot's patch a session
// keeps when its caller sets no bound: a patch past it is recorded as
// too large, with its size and no data.
const DefaultSnapshotMax int64 = config.DefaultSnapshotMaxBytes

// StartBase is the prefix of a start snapshot's base: the start
// snapshot is the tree as the run found it, against the commit that
// follows the prefix.
const StartBase = "start:"

// snapshotTimeout bounds each read of the workspace's change.
const snapshotTimeout = 2 * time.Minute

// snapshots is a session's start, as StartSnapshot read it.
type snapshots struct {
	mu sync.Mutex
	// on is set once StartSnapshot ran in a git tree, or failed to
	// read one: EndSnapshot then records a snapshot.
	on  bool
	max int64
	// start is the tree as the run found it; err why it could not be
	// read.
	start tool.GitChange
	err   error
	// bases are the commits of the other checkouts the session follows
	// as the run found them, by path: what each one's snapshot at the
	// end is against.
	bases map[string]string
	// noGit is set once a read found no git in the workspace, which is
	// said once, after which a run starts no snapshot.
	noGit bool
	// done is set once EndSnapshot has written the run's snapshots.
	done bool
}

// pending is whether StartSnapshot began a snapshot that EndSnapshot
// has not written yet.
func (sn *snapshots) pending() bool {
	sn.mu.Lock()
	defer sn.mu.Unlock()
	return sn.on && !sn.done
}

// vcs is the env entry's vcs for the start, nil when there is none.
func (sn *snapshots) vcs() *agentsession.VCS {
	sn.mu.Lock()
	defer sn.mu.Unlock()
	if !sn.on || sn.err != nil || !sn.start.Commit {
		return nil
	}
	return &agentsession.VCS{System: "git", Revision: sn.start.Base, Dirty: sn.start.Size > 0}
}

// StartSnapshot reads where the workspace starts, before a run, so
// that EndSnapshot records the change the run left: the commit the
// git tree is on, which the run's env entry records as its vcs (with
// whether the tree was dirty), and, for a dirty tree, the change
// already there. max bounds the bytes of each snapshot's patch kept; 0
// is DefaultSnapshotMax. A workspace that is not a git tree, or a
// session nothing is recorded for, starts none and is no error. A
// workspace with no git installed starts none either, and the first
// run that finds so returns an error saying it (wrapping
// tool.ErrNoGit), the runs after it none. Any other error reading the
// tree is returned, and EndSnapshot records it as the snapshot's. The
// commits of the other checkouts the session follows are read too, for
// EndSnapshot's snapshot of each one the run changes.
func (s *Session) StartSnapshot(ctx context.Context, max int64) error {
	if !s.recorded {
		return nil
	}
	if max <= 0 {
		max = DefaultSnapshotMax
	}
	ctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
	defer cancel()
	sn := &s.snap
	sn.mu.Lock()
	quiet := sn.noGit
	sn.mu.Unlock()
	if quiet {
		return nil
	}
	c, err := tool.GitDiff(ctx, s.files, "", max)
	if err != nil && errors.Is(err, fs.ErrNotExist) {
		sn.mu.Lock()
		sn.on, sn.noGit = false, true
		sn.mu.Unlock()
		return fmt.Errorf("snapshot: %w: %v", tool.ErrNoGit, err)
	}
	s.readCheckouts(ctx)
	bases := s.checkoutRevisions()
	sn.mu.Lock()
	defer sn.mu.Unlock()
	sn.on, sn.max, sn.start, sn.err, sn.bases, sn.done = c.Repo || err != nil, max, c, err, bases, false
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	return nil
}

// SnapshotWritten is a snapshot entry EndSnapshot appended: its ID,
// and the snapshot as recorded (its data left out).
type SnapshotWritten struct {
	// Path is the checkout the snapshot is of, as the env entry names
	// it; empty for the workspace's root.
	Path   string
	Entry  string
	Base   string
	Digest string
	// Size is the patch's length in bytes, 0 when it could not be read.
	Size  int64
	Error string
}

// Snapshots are what EndSnapshot appended: the run's final snapshot,
// and the start's when the tree was dirty at the start, of the
// workspace's root; and the final snapshot of each other checkout the
// session follows that the run changed, in the order it was first
// followed.
type Snapshots struct {
	Final, Start *SnapshotWritten
	Checkouts    []*SnapshotWritten
}

// EndSnapshot reads the change the workspace holds against the commit
// StartSnapshot read, and appends it after the run's last entry as an
// agenteval snapshot entry (agenteval.SnapshotNS), its base the commit,
// so that agenteval.ResultAt reads it as the run's. When the tree was
// dirty at the start, the start's change is appended first, its base
// StartBase and the commit: the run's own change is the final one less
// it. A change that could not be read, or is over the bound, is
// recorded with Error saying why and no data; one over the bound keeps
// its base and its size. It does nothing, and
// returns nil Snapshots, when StartSnapshot started none. The error is
// an append's; a snapshot that could not be read is no error.
func (s *Session) EndSnapshot(ctx context.Context) (*Snapshots, error) {
	sn := &s.snap
	sn.mu.Lock()
	on, max, start, startErr, bases := sn.on, sn.max, sn.start, sn.err, sn.bases
	sn.mu.Unlock()
	if !on {
		return nil, nil
	}
	// The checkouts' env entry the run's end held back follows the
	// snapshots, however they end.
	defer func() {
		sn.mu.Lock()
		sn.done = true
		sn.mu.Unlock()
		s.writeCheckouts(context.WithoutCancel(ctx))
	}()
	var final agenteval.Snapshot
	var size int64
	switch {
	case startErr != nil:
		final.Error = startErr.Error()
	case start.Withheld != "":
		final.Error = "withheld: the repository's own config names " + start.Withheld
	default:
		rctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
		c, err := tool.GitDiff(rctx, s.files, start.Base, max)
		cancel()
		final, size = snapshotOf(c, err, start.Base, max)
	}
	target := ""
	if live := s.Kit.Session(); live != nil {
		target = live.Leaf()
	}
	if target == "" {
		return nil, errors.New("snapshot: the session holds no entry to name as the run")
	}
	out := &Snapshots{}
	if startErr == nil && start.Withheld == "" && start.Size > 0 {
		snap, n := snapshotOf(start, nil, StartBase+start.Base, max)
		w, err := s.appendSnapshot(ctx, snap, n, target)
		if err != nil {
			return out, err
		}
		out.Start = w
	}
	w, err := s.appendSnapshot(ctx, final, size, target)
	if err != nil {
		return out, err
	}
	out.Final = w
	// Each other checkout the run changed: its change against the
	// commit the run found it on, or the one the session first followed
	// it at, for one the run made.
	s.readCheckouts(ctx)
	for _, ch := range s.changedSince(bases) {
		rctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
		c, err := tool.GitDiffIn(rctx, s.files, ch.dir, ch.base, max)
		cancel()
		snap, n := snapshotOf(c, err, ch.base, max)
		snap.Path = ch.path
		w, err := s.appendSnapshot(ctx, snap, n, target)
		if err != nil {
			return out, err
		}
		out.Checkouts = append(out.Checkouts, w)
	}
	return out, nil
}

// snapshotOf is the snapshot of a change GitDiff read, against base,
// with the patch's size.
func snapshotOf(c tool.GitChange, err error, base string, max int64) (agenteval.Snapshot, int64) {
	switch {
	case err != nil:
		return agenteval.Snapshot{Error: err.Error()}, 0
	case c.Withheld != "":
		return agenteval.Snapshot{Error: "withheld: the repository's own config names " + c.Withheld}, 0
	case !c.Repo:
		return agenteval.Snapshot{Error: "the workspace is no longer in a git work tree"}, 0
	case c.Data == nil && c.Size > 0:
		// The entry keeps what the patch applies to and how large it
		// was, with no patch (agenteval#79).
		return agenteval.Snapshot{Base: base, Size: c.Size, Error: fmt.Sprintf("too large: the patch is %d bytes, over the bound of %d (snapshot_max_bytes)", c.Size, max)}, c.Size
	}
	return agenteval.Snapshot{Data: c.Data, Base: base}, c.Size
}

// appendSnapshot appends snap as a snapshot entry of the run whose last
// entry is target.
func (s *Session) appendSnapshot(ctx context.Context, snap agenteval.Snapshot, size int64, target string) (*SnapshotWritten, error) {
	e, err := agenteval.NewSnapshot(snap, target, "")
	if err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	id, err := s.store.Append(ctx, s.ID(), e)
	if err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	rec, _ := agenteval.ReadSnapshot(e)
	return &SnapshotWritten{Path: rec.Path, Entry: id, Base: rec.Base, Digest: rec.Digest, Size: size, Error: rec.Error}, nil
}
