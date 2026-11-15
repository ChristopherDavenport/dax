package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentmemory/filestore"
	"github.com/ChristopherDavenport/agenttool"
)

// racing is a store whose first Get or Forget of an entry is preceded
// (a Forget) or followed (a Get), once, by a save of other content to
// it: the save a conversation makes after the entry was read and before
// it is forgotten.
type racing struct {
	agentmemory.Store
	scope agentmemory.Scope
	name  string
	armed bool
}

func (r *racing) save(ctx context.Context, scope agentmemory.Scope, name string) error {
	if !r.armed || scope != r.scope || name != r.name {
		return nil
	}
	r.armed = false
	_, err := r.Store.Put(ctx, agentmemory.Entry{Scope: scope, Name: name, Content: "saved in between"})
	return err
}

func (r *racing) Get(ctx context.Context, scope agentmemory.Scope, name string) (*agentmemory.Entry, error) {
	e, err := r.Store.Get(ctx, scope, name)
	if serr := r.save(ctx, scope, name); serr != nil {
		return nil, serr
	}
	return e, err
}

func (r *racing) Forget(ctx context.Context, scope agentmemory.Scope, name string, opts ...agentmemory.PutOption) (*agentmemory.Change, error) {
	if err := r.save(ctx, scope, name); err != nil {
		return nil, err
	}
	return r.Store.Forget(ctx, scope, name, opts...)
}

// The pass's forget is conditional on the hash it read when it decided
// the call (agentmemory#53): a save landing after the check is never
// forgotten, where a read, a compare and a forget would forget it.
func TestThePassForgetsOnlyWhatItRead(t *testing.T) {
	ctx := context.Background()
	fs, err := filestore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := fs.Put(ctx, agentmemory.Entry{Scope: "user", Name: "errors", Content: "Wrap errors with %w."})
	if err != nil {
		t.Fatal(err)
	}
	seen := &seenHashes{calls: map[string]string{"c1": c.Entry.Hash}}
	var conflicts int
	r := &racing{Store: fs, scope: "user", name: "errors", armed: true}
	s := &passStore{Store: r, seen: seen, conflict: func(agentmemory.Scope, string) { conflicts++ }}
	cctx := agenttool.WithCall(ctx, agenttool.Call{ID: "c1"})
	if _, err := s.Forget(cctx, "user", "errors"); !errors.Is(err, agentmemory.ErrConflict) {
		t.Errorf("Forget = %v, want a conflict", err)
	}
	e, err := fs.Get(ctx, "user", "errors")
	if err != nil || e.Content != "saved in between" {
		t.Errorf("the entry after the forget: %+v, %v; want the save made in between kept", e, err)
	}
	if conflicts != 1 {
		t.Errorf("the conflict was reported %d times, want once", conflicts)
	}
}

// /memory undo forgets an entry the pass made only while it holds what
// the pass left (agentmemory#53): a save landing after its check is
// left, and named.
func TestUndoForgetsOnlyWhatThePassLeft(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	fs, err := filestore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	c, err := fs.Put(agentmemory.WithSession(ctx, "pass-1"), agentmemory.Entry{Scope: "user", Name: "errors", Content: "Wrap errors with %w."})
	if err != nil {
		t.Fatal(err)
	}
	if err := appendLedger(dir, ledgerLine{Session: "pass-1", Project: "project-x-00000000", From: c.Seq - 1, To: c.Seq}); err != nil {
		t.Fatal(err)
	}
	defer func(open func(string) (agentmemory.Store, error)) { openUndoStore = open }(openUndoStore)
	openUndoStore = func(string) (agentmemory.Store, error) {
		return &racing{Store: fs, scope: "user", name: "errors", armed: true}, nil
	}
	out, err := Undo(ctx, dir, "project-x-00000000")
	if err != nil {
		t.Fatal(err)
	}
	e, err := fs.Get(ctx, "user", "errors")
	if err != nil || e.Content != "saved in between" {
		t.Errorf("after the undo (%q): %+v, %v; want the save made in between kept", out, e, err)
	}
}
