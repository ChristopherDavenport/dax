package dax

import (
	"bufio"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentconsole/console"
	"github.com/ChristopherDavenport/agentsession"

	"github.com/ChristopherDavenport/dax/agent"
)

// #13: /new in the REPL moves to a fresh session over the live
// settings, and the old one is closed; whoever closes the session at
// the end is told of the new one.
func TestTheREPLMovesToANewSession(t *testing.T) {
	ctx := context.Background()
	sess, _ := printSession(t, ctx, &steps{})
	pr, pw := io.Pipe()
	out := &syncBuf{}
	var cur *agent.Session
	f := &replFront{info: frontInfo{Name: "dax", switched: func(n *agent.Session) { cur = n }}, in: bufio.NewScanner(pr), out: out}
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx, sess) }()
	pw.Write([]byte("/model other\n"))
	waitIn(t, out, "[model other")
	pw.Write([]byte("/new\n"))
	waitIn(t, out, "[session ")
	pw.Write([]byte("hello\n"))
	waitIn(t, out, "all done")
	pw.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if cur == nil {
		t.Fatal("the front moved to no session")
	}
	t.Cleanup(func() { cur.Close() })
	if cur.ID() == sess.ID() {
		t.Fatal("/new stayed in the same session")
	}
	if cur.Model() != "other" {
		t.Errorf("model %q after /new, want other", cur.Model())
	}
	if got, ok := wordsOf(t, cur)["hello"]; !ok || got != agentsession.WordsPerson {
		t.Errorf("the new session's record: hello %v (found %v), want the person's", got, ok)
	}
	// The old session was closed by the move: closing it again is a no-op.
	if err := sess.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
}

// /new and /clear wait for the run, take no argument, and are refused
// over dax attach, whose views all act on the one served conversation.
func TestNewIsRefusedWhereItCannotMove(t *testing.T) {
	ctx := context.Background()
	fc := &fakeControls{}
	for _, tc := range []struct {
		line    string
		running bool
		want    string
	}{
		{"/new", false, "not offered over dax attach"},
		{"/clear", false, "not offered over dax attach"},
		{"/new x", false, "usage: /new"},
		{"/new", true, "waits for the run to end"},
	} {
		_, err := command(ctx, nil, fc, tc.line, tc.running)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s (running %v): %v, want %q", tc.line, tc.running, err, tc.want)
		}
	}
}

// In the terminal client, /new ends the client over the old session
// with the new one to move to, which Run enters the client again over.
func TestTheTUIsNewEndsTheClientWithTheSessionToMoveTo(t *testing.T) {
	ctx := context.Background()
	sess, _ := printSession(t, ctx, &steps{})
	cctx, leave := context.WithCancel(ctx)
	defer leave()
	sw := &switching{Session: sess, leave: leave}
	submit := submitLine(sess, sw, func() { t.Error("/new quit the client") })
	if _, err := submit(cctx, console.Line{Text: "/new"}); err != nil {
		t.Fatal(err)
	}
	n := sw.next.Load()
	if n == nil {
		t.Fatal("no session to move to")
	}
	defer n.Close()
	if n.ID() == sess.ID() {
		t.Error("the session to move to is the old one")
	}
	if cctx.Err() == nil {
		t.Error("the client over the old session goes on")
	}
}
