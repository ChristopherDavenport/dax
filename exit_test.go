package dax

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentconsole/client"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/ext/agents"
	"github.com/ChristopherDavenport/dax/ext/coding"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/policy"
)

// Each way a run ends has its own exit status, so a harness that reads
// $? knows what happened (#138): 0 done, 1 an error, and one each for
// stopped, input required and aborted.
func TestExitStatusOfARunsEnd(t *testing.T) {
	for _, tc := range []struct {
		reason agentturn.Reason
		want   int
	}{
		{agentturn.ReasonDone, 0},
		{agentturn.ReasonError, 1},
		{agentturn.ReasonStopped, exitStopped},
		{agentturn.ReasonInputRequired, exitInputRequired},
		{agentturn.ReasonAborted, exitAborted},
	} {
		if got := exitStatus(&agentturn.RunEnd{Reason: tc.reason}); got != tc.want {
			t.Errorf("%s: exit %d, want %d", tc.reason, got, tc.want)
		}
	}
	seen := map[int]agentturn.Reason{}
	for _, r := range []agentturn.Reason{agentturn.ReasonError, agentturn.ReasonStopped, agentturn.ReasonInputRequired, agentturn.ReasonAborted} {
		s := exitStatus(&agentturn.RunEnd{Reason: r})
		if other, ok := seen[s]; ok {
			t.Errorf("%s and %s both exit %d", r, other, s)
		}
		seen[s] = r
	}
}

// sessionOver is a recorded session for -p over streamer.
func sessionOver(t *testing.T, streamer openresponses.Streamer) (*agent.Session, string) {
	t.Helper()
	base := t.TempDir()
	o := agent.Options{
		Model: "scripted", Streamer: streamer, Dir: filepath.Join(base, "project"), Root: filepath.Join(base, "sessions"),
		UserDir:    filepath.Join(base, "user"),
		Extensions: []extension.Extension{coding.New(0), agents.New(agents.Options{})},
		Policy:     &policy.Settings{Builtin: true, Fallback: "ask"},
	}
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sess, err := agent.New(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess, o.Root
}

// runEndOf is the last run end on the session's record.
func runEndOf(t *testing.T, sess *agent.Session) *agentsession.RunEntry {
	t.Helper()
	r, err := client.RecordOf(sess.Recorder())
	if err != nil {
		t.Fatal(err)
	}
	rec, err := r.Read(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	var end *agentsession.RunEntry
	for _, e := range rec.Path(rec.Leaf()) {
		if r, ok := e.(*agentsession.RunEntry); ok && r.IsEnd() {
			end = r
		}
	}
	if end == nil {
		t.Fatal("no run end on the record")
	}
	return end
}

// -timeout ends the run aborted, its cause the timeout, on the record
// and in the exit status; a run that finishes in time exits 0.
func TestPrintTimeoutAbortsTheRunWithItsCause(t *testing.T) {
	g := &gated{steps: &steps{}, release: make(chan struct{})}
	t.Cleanup(g.let)
	sess, _ := sessionOver(t, g)
	f := &printFront{info: frontInfo{Name: "dax", Prompt: "go", Timeout: 200 * time.Millisecond}, in: bufio.NewScanner(strings.NewReader(""))}
	done := make(chan error, 1)
	go func() { done <- f.run(t.Context(), sess, sess) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("-timeout did not end the run")
	}
	var ended *runEnded
	if !errors.As(err, &ended) || ended.status != exitAborted {
		t.Fatalf("run: %v, want the aborted exit status %d", err, exitAborted)
	}
	// The record names an aborted run interrupted (agentsession's word).
	if end := runEndOf(t, sess); end.Reason != agentsession.ReasonInterrupted || !strings.Contains(end.Ref, "timeout") {
		t.Errorf("the record ends %q with %q, want aborted with the timeout", end.Reason, end.Ref)
	}

	done2, _ := sessionOver(t, &steps{})
	f = &printFront{info: frontInfo{Name: "dax", Prompt: "go", Timeout: time.Minute}, in: bufio.NewScanner(strings.NewReader(""))}
	if err := f.run(t.Context(), done2, done2); err != nil {
		t.Errorf("a run done in time: %v, want exit 0", err)
	}
}

// -result writes the run's end and usage as JSON, matching the record,
// and the run then exits 0 however it ended, for a harness that counts
// any other status as its own failure.
func TestPrintResultMatchesTheRecord(t *testing.T) {
	g := &gated{steps: &steps{}, release: make(chan struct{})}
	t.Cleanup(g.let)
	sess, _ := sessionOver(t, g)
	path := filepath.Join(t.TempDir(), "result.json")
	f := &printFront{info: frontInfo{Name: "dax", Prompt: "go", Timeout: 200 * time.Millisecond, Result: path}, in: bufio.NewScanner(strings.NewReader(""))}
	if err := f.run(t.Context(), sess, sess); err != nil {
		t.Fatalf("run with -result: %v, want exit 0", err)
	}
	var got runResult
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("%s: %v", data, err)
	}
	end := runEndOf(t, sess)
	// The run's end as agentturn says it, the record's interrupted.
	if got.Session != sess.ID() || got.Reason != string(agentturn.ReasonAborted) || end.Reason != agentsession.ReasonInterrupted || got.Error != end.Ref || got.Exit != exitAborted || !strings.Contains(got.Error, "timeout") {
		t.Errorf("result %s, want session %s, aborted as the record's %s with %q, exit %d", data, sess.ID(), end.Reason, end.Ref, exitAborted)
	}

	finished, _ := sessionOver(t, &steps{})
	f = &printFront{info: frontInfo{Name: "dax", Prompt: "go", Result: path}, in: bufio.NewScanner(strings.NewReader(""))}
	if err := f.run(t.Context(), finished, finished); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	got = runResult{}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Reason != string(agentturn.ReasonDone) || got.Exit != 0 || got.Usage.ModelCalls == 0 {
		t.Errorf("result %s, want done, exit 0 and the model calls counted", data)
	}
}

// -timeout and -result are -p's.
func TestTimeoutAndResultGoWithPrint(t *testing.T) {
	for _, flag := range [][]string{{"-timeout", "1m"}, {"-result", "r.json"}} {
		err := run(t.Context(), append(flag, "-sessions", ""), program{name: "dax", version: "test"})
		if err == nil || !strings.Contains(err.Error(), flag[0]+" goes with -p") {
			t.Errorf("%v without -p: %v", flag, err)
		}
	}
}
