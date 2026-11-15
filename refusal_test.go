package dax

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/internal/acp"
)

// stoppedShort answers every request with a message its provider ended
// incomplete for reason: a refusal, with Anthropic's category in the
// details, for content_filter; cut text for max_output_tokens.
type stoppedShort struct {
	reason openresponses.IncompleteReason
}

const refusalText = "I can't help with that."

func (m stoppedShort) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	em := openresponses.NewEmitter(withCategory{sink}, openresponses.NewResponse(req))
	if err := em.Start(); err != nil {
		return err
	}
	w, err := em.Message(openresponses.PhaseFinalAnswer)
	if err != nil {
		return err
	}
	if m.reason == openresponses.IncompleteReasonContentFilter {
		err = w.Refusal(refusalText)
	} else {
		err = w.Text("Once upon")
	}
	if err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return em.Incomplete(m.reason)
}

// withCategory adds the refusal's category to a content filter's
// incomplete details, as the anthropic adapter does.
type withCategory struct{ openresponses.EventSink }

func (s withCategory) Send(ev openresponses.StreamEvent) error {
	if e, ok := ev.(*openresponses.ResponseIncompleteEvent); ok {
		if d := e.Response.IncompleteDetails; d != nil && d.Reason == openresponses.IncompleteReasonContentFilter {
			d.Extra = map[string]any{"anthropic.refusal_category": "cyber"}
		}
	}
	return s.EventSink.Send(ev)
}

// -p ends a run the provider's content filter refused with an exit
// status of its own and reason refused in -result, its refusal and the
// details beside it; an answer the output limit cut has its own, cut
// (#206).
func TestPrintSaysARefusalAndACut(t *testing.T) {
	for _, tc := range []struct {
		reason  openresponses.IncompleteReason
		status  int
		said    string
		refusal string
	}{
		{openresponses.IncompleteReasonContentFilter, exitRefused, "refused", refusalText},
		{openresponses.IncompleteReasonMaxOutputTokens, exitCut, "cut", ""},
	} {
		t.Run(string(tc.reason), func(t *testing.T) {
			sess, _ := sessionOver(t, stoppedShort{reason: tc.reason})
			f := &printFront{info: frontInfo{Name: "dax", Prompt: "go"}, in: bufio.NewScanner(strings.NewReader(""))}
			var ended *runEnded
			if err := f.run(t.Context(), sess, sess); !errors.As(err, &ended) || ended.status != tc.status {
				t.Fatalf("run: %v, want exit %d", err, tc.status)
			}
			again, _ := sessionOver(t, stoppedShort{reason: tc.reason})
			path := filepath.Join(t.TempDir(), "result.json")
			f = &printFront{info: frontInfo{Name: "dax", Prompt: "go", Result: path}, in: bufio.NewScanner(strings.NewReader(""))}
			if err := f.run(t.Context(), again, again); err != nil {
				t.Fatalf("run with -result: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Reason, Cause, Refusal string
				Exit                   int
				Incomplete             map[string]any
			}
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if got.Reason != tc.said || got.Cause != string(tc.reason) || got.Exit != tc.status || got.Refusal != tc.refusal || got.Incomplete["reason"] != string(tc.reason) {
				t.Errorf("result %s, want reason %s, cause %s, exit %d, refusal %q and the details", data, tc.said, tc.reason, tc.status, tc.refusal)
			}
			if tc.status == exitRefused && got.Incomplete["anthropic.refusal_category"] != "cyber" {
				t.Errorf("result %s, want the refusal's category", data)
			}
		})
	}
}

// The REPL prints a refusal's text and category, and ends the run's
// line refused (#206).
func TestTheREPLPrintsARefusal(t *testing.T) {
	ctx := context.Background()
	sess, _ := sessionOver(t, stoppedShort{reason: openresponses.IncompleteReasonContentFilter})
	pr, pw := io.Pipe()
	out := &syncBuf{}
	f := &replFront{info: frontInfo{Name: "dax"}, in: bufio.NewScanner(pr), out: out}
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx, sess) }()
	pw.Write([]byte("go\n"))
	waitIn(t, out, "[refused ·")
	if !strings.Contains(out.String(), "[refused: cyber] "+refusalText) {
		t.Errorf("the REPL printed:\n%s\nwant the refusal and its category", out.String())
	}
	pw.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// ACP stops a prompt the provider's content filter refused with
// refusal, and one whose answer the output limit cut with max_tokens
// (#206).
func TestACPSaysARefusalAndACut(t *testing.T) {
	for _, tc := range []struct {
		reason openresponses.IncompleteReason
		want   string
	}{
		{openresponses.IncompleteReasonContentFilter, acp.StopRefusal},
		{openresponses.IncompleteReasonMaxOutputTokens, acp.StopMaxTokens},
	} {
		t.Run(string(tc.reason), func(t *testing.T) {
			sess, _ := sessionOver(t, stoppedShort{reason: tc.reason})
			e := startACP(t, sess, sess.Dir())
			e.call(acp.MethodInitialize, acp.InitializeRequest{ProtocolVersion: 1})
			var created acp.NewSessionResponse
			json.Unmarshal(e.call(acp.MethodSessionNew, acp.NewSessionRequest{CWD: sess.Dir()}).Result, &created)
			var res acp.PromptResponse
			json.Unmarshal(e.call(acp.MethodSessionPrompt, acp.PromptRequest{SessionID: created.SessionID, Prompt: []acp.ContentBlock{acp.Text("go")}}).Result, &res)
			if res.StopReason != tc.want {
				t.Errorf("stop reason %q, want %q", res.StopReason, tc.want)
			}
			// The editor is shown what the provider refused with, and
			// its category, before the stop (#206's review, L6).
			if us := e.updates(); tc.reason == openresponses.IncompleteReasonContentFilter && !has(us, acp.UpdateAgentMessage, "[refused: cyber] "+refusalText) {
				t.Errorf("updates %+v, want the refusal as the agent's message", us)
			}
			e.stop()
		})
	}
}
