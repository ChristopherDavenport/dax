package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentmemory/filestore"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/ext/memory"
	"github.com/ChristopherDavenport/dax/extension"
)

// logLines collects a session's notes.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) log(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logLines) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// settled waits for the AfterRun hooks of the runs that ended, so a
// pass due after one is due before the next prompt.
func settled(s *Session) { s.after.wg.Wait() }

// withPass is o with dax-memory over a store of its own under the
// background pass b, the pass's model pass; it returns the store's
// directory.
func withPass(t *testing.T, o Options, b memory.Background, pass openresponses.Streamer) (Options, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "memory")
	var exts []extension.Extension
	for _, e := range o.Extensions {
		if e.Name != memory.Name {
			exts = append(exts, e)
		}
	}
	o.Extensions = append(exts, memory.New(dir, memory.WithBackground(b)))
	o.SubagentStreamer = pass
	o.MemoryDir = dir
	return o, dir
}

// An auto pass after a run that called a tool writes unasked, as a
// subsession of the conversation the session's store lists under it;
// the person is told what it wrote, and /memory undo reverts it.
// (#160 b)
func TestAnAutoPassWritesAsASubsessionAndUndoRevertsIt(t *testing.T) {
	ctx := context.Background()
	o := options(t, &scripted{calls: [][2]string{{"ls", `{}`}}})
	o.Policy = confirmPolicy(t)
	var logs logLines
	o.Log = logs.log
	o, dir := withPass(t, o, memory.Background{Mode: memory.BackgroundAuto},
		&scripted{calls: [][2]string{{"memory_save", `{"scope":"user","name":"errors","content":"Wrap errors with %w."}`}}})
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prompt(ctx, openresponses.UserText("list the files")); err != nil {
		t.Fatal(err)
	}
	id, ws := s.ID(), s.Info().Workspace
	// The close waits for the pass.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	mem, err := filestore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, err := mem.Get(ctx, "user", "errors")
	if err != nil || e.Content != "Wrap errors with %w." {
		t.Fatalf("the pass's entry: %+v, %v; logs:\n%s", e, err, logs.all())
	}
	if !strings.Contains(logs.all(), "memory updated: saved user/errors") {
		t.Errorf("the person was not told what the pass wrote:\n%s", logs.all())
	}
	st, err := cas.Open(o.Root, cas.WithReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	child, err := st.Open(ctx, agentsession.SpawnedSessionID(id, "dax-memory-pass"))
	st.Close()
	if err != nil {
		t.Fatalf("the pass's subsession: %v", err)
	}
	if child.Header().ParentSession != id {
		t.Errorf("the pass's session names parent %q, want %s", child.Header().ParentSession, id)
	}
	out, err := memory.Undo(ctx, dir, memory.WorkspaceScope(ws, o.Dir))
	if err != nil || !strings.Contains(out, "forgot user/errors") {
		t.Fatalf("undo: %q, %v", out, err)
	}
	if _, err := mem.Get(ctx, "user", "errors"); err == nil {
		t.Error("the entry is still there after the undo")
	}
	if out, _ := memory.Undo(ctx, dir, memory.WorkspaceScope(ws, o.Dir)); !strings.Contains(out, "no memory pass") {
		t.Errorf("a second undo: %q, want nothing left", out)
	}
}

// Under auto the pass still asks before it changes an entry no pass
// wrote, the person's or the conversation's model's; with nobody to ask
// the entry stays as it was. (#160 b, hard limit 3)
func TestAnAutoPassAsksBeforeItChangesAnEntryItDidNotWrite(t *testing.T) {
	ctx := context.Background()
	o := options(t, &scripted{calls: [][2]string{{"ls", `{}`}}})
	o.Policy = confirmPolicy(t)
	o, dir := withPass(t, o, memory.Background{Mode: memory.BackgroundAuto},
		&scripted{calls: [][2]string{{"memory_save", `{"scope":"user","name":"style","content":"tabs"}`}}})
	mem, err := filestore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Put(ctx, agentmemory.Entry{Scope: "user", Name: "style", Content: "spaces"}); err != nil {
		t.Fatal(err)
	}
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prompt(ctx, openresponses.UserText("list the files")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if e, err := mem.Get(ctx, "user", "style"); err != nil || e.Content != "spaces" {
		t.Errorf("the person's entry: %+v, %v; want it left as it was", e, err)
	}
}

// Under propose the pass waits for the person's next prompt and asks
// about each write through the question a sub-agent's asked call puts;
// a yes writes it, and "yes, always" (a session grant of the offered
// rule) lets the next pass write unasked. With nobody answering, the
// write is not made. (#160 c)
func TestAProposingPassAsksAtTheNextPrompt(t *testing.T) {
	ctx := context.Background()
	o := options(t, &scripted{calls: [][2]string{{"ls", `{}`}}})
	o.Policy = confirmPolicy(t)
	var logs logLines
	o.Log = logs.log
	o, dir := withPass(t, o, memory.Background{Mode: memory.BackgroundPropose},
		&scripted{calls: [][2]string{{"memory_save", `{"scope":"user","name":"errors","content":"Wrap errors with %w."}`}}})
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mem, err := filestore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prompt(ctx, openresponses.UserText("list the files")); err != nil {
		t.Fatal(err)
	}
	settled(s)
	if _, err := mem.Get(ctx, "user", "errors"); err == nil {
		t.Fatal("a proposing pass wrote before the person was back")
	}
	// The next prompt, with nobody answering: the write is not made.
	if _, err := s.Prompt(ctx, openresponses.UserText("again")); err != nil {
		t.Fatal(err)
	}
	settled(s)
	if _, err := mem.Get(ctx, "user", "errors"); err == nil {
		t.Fatal("the pass wrote with nobody to ask")
	}
	// The next, answered "yes, always": the session grant, then the yes.
	var asked []string
	var offered string
	rules := Rules{
		Reply: func(q agentturn.Question) agenttool.Answer {
			asked = append(asked, q.Elicitation.Message)
			if o := s.QuestionOffer(ctx, q); o.Rule != "" {
				offered = o.Rule
				if err := s.GrantQuestion(ctx, q, o.Rule); err != nil {
					t.Error(err)
				}
			}
			return agenttool.Answer{Action: agenttool.ActionAccept}
		},
		Permit: func(*openresponses.FunctionCall, string) PermitAnswer { return PermitAnswer{Allow: true} },
	}
	if _, err := Drive(ctx, s, rules, openresponses.UserText("once more")); err != nil {
		t.Fatal(err)
	}
	settled(s)
	if len(asked) != 1 || !strings.Contains(asked[0], "the memory pass sub-agent asks") || offered != "memory_save" {
		t.Fatalf("asked %q, offered %q; want the pass's write asked once, memory_save offered", asked, offered)
	}
	if e, err := mem.Get(ctx, "user", "errors"); err != nil || e.Content != "Wrap errors with %w." {
		t.Fatalf("the approved write: %+v, %v", e, err)
	}
	if _, err := mem.Forget(ctx, "user", "errors"); err != nil {
		t.Fatal(err)
	}
	// The pass after the next run writes unasked under the grant.
	asked = nil
	if _, err := Drive(ctx, s, rules, openresponses.UserText("and again")); err != nil {
		t.Fatal(err)
	}
	settled(s)
	if len(asked) != 0 {
		t.Errorf("asked %q after yes, always", asked)
	}
	if _, err := mem.Get(ctx, "user", "errors"); err != nil {
		t.Errorf("the pass under the grant did not write: %v\n%s", err, logs.all())
	}
}
