package dax

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/internal/config"
)

// #164: /name names the session and -list shows it; /resume lists this
// project's sessions, newest first and named, and /resume <name> moves
// the REPL to that session's head, over the live settings.
func TestTheREPLNamesAndResumesASession(t *testing.T) {
	ctx := context.Background()
	sess, dir := printSession(t, ctx, &steps{})
	first := sess.ID()
	pr, pw := io.Pipe()
	out := &syncBuf{}
	var cur *agent.Session
	f := &replFront{info: frontInfo{Name: "dax", switched: func(n *agent.Session) { cur = n }}, in: bufio.NewScanner(pr), out: out}
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx, sess) }()
	pw.Write([]byte("the auth work\n"))
	waitIn(t, out, "tokens]")
	pw.Write([]byte("/name auth refactor\n"))
	waitIn(t, out, `[session named "auth refactor"]`)
	pw.Write([]byte("/new\n"))
	waitIn(t, out, "[session ")
	pw.Write([]byte("/resume\n"))
	waitIn(t, out, `"auth refactor"`)
	pw.Write([]byte("/model other\n"))
	waitIn(t, out, "[model other")
	pw.Write([]byte("/resume auth\n"))
	waitIn(t, out, "[session "+first+"]")
	pw.Write([]byte("go on\n"))
	for deadline := time.Now().Add(20 * time.Second); strings.Count(out.String(), "tokens]") < 2; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the resumed session's run did not end:\n%s", out.String())
		}
	}
	pw.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cur.Close() })
	if cur.ID() != first {
		t.Fatalf("resumed %s, want %s", cur.ID(), first)
	}
	if cur.Model() != "other" {
		t.Errorf("model %q after /resume, want the live other", cur.Model())
	}
	words := wordsOf(t, cur)
	if _, ok := words["the auth work"]; !ok {
		t.Errorf("the resumed session's path holds no earlier prompt: %v", words)
	}
	if got, ok := words["go on"]; !ok || got != agentsession.WordsPerson {
		t.Errorf("the resumed session's new prompt: %v (found %v)", got, ok)
	}
	// /resume's listing: newest first, the session the REPL was on marked.
	listing := out.String()
	if !regexp.MustCompile(`\*  1  \S+ \S+  \S+\n   2  \S+ \S+  ` + first + `  "auth refactor"\n`).MatchString(listing) {
		t.Errorf("the named session is not second, under the new one, marked:\n%s", listing)
	}
	cur.Close()
	var b bytes.Buffer
	if err := runList(ctx, &b, filepath.Join(filepath.Dir(dir), "sessions"), dir, "", config.Flags{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), first+`  "auth refactor"`) {
		t.Errorf("-list:\n%s\nwant the name after its ID", b.String())
	}
}

// /resume and /name are not offered over dax attach; a reference that
// names no session, or several, is refused with what it matched.
func TestResumeRefusesWhatItCannotPick(t *testing.T) {
	ctx := context.Background()
	for _, line := range []string{"/resume", "/resume x", "/name x"} {
		if _, err := command(ctx, nil, &fakeControls{}, line, false); err == nil || !strings.Contains(err.Error(), "dax attach") {
			t.Errorf("%s over attach: %v", line, err)
		}
	}
	sums := []agentsession.Summary{
		{Header: agentsession.Header{ID: "aaaa-1"}, Name: "auth refactor"},
		{Header: agentsession.Header{ID: "aaaa-2"}, Name: "auth tests"},
		{Header: agentsession.Header{ID: "bbbb-3"}},
	}
	for _, tc := range []struct{ ref, id, err string }{
		{"1", "aaaa-1", ""},
		{"3", "bbbb-3", ""},
		{"4", "", "no session 4"},
		{"aaaa-2", "aaaa-2", ""},
		{"bbbb", "bbbb-3", ""},
		{"aaaa", "", "the start of the ID of 2 sessions"},
		{"AUTH TESTS", "aaaa-2", ""},
		{"refactor", "aaaa-1", ""},
		{"auth", "", "part of the name of 2 sessions"},
		{"zzz", "", "no session in this project"},
	} {
		id, err := pickSession(sums, tc.ref)
		if id != tc.id || (tc.err == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), tc.err) {
			t.Errorf("pickSession(%q) = %q, %v; want %q, %q", tc.ref, id, err, tc.id, tc.err)
		}
	}
}
