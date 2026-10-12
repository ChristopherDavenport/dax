package toolrender

import (
	"context"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/ChristopherDavenport/agentconsole/view"
	"github.com/ChristopherDavenport/agenttool"
	a2atool "github.com/ChristopherDavenport/agentturn/tools/a2a"
	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2aclient"
	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/a2aproject/a2a-go/a2asrv/eventqueue"
)

// answering answers a request with a two-line report, and one that says
// "ask" with a question it ends input-required on.
type answering struct{}

func (answering) Execute(ctx context.Context, reqCtx *a2asrv.RequestContext, q eventqueue.Queue) error {
	state, reply := a2a.TaskStateCompleted, "Painted the fence.\n\nTwo coats, white."
	if t, ok := reqCtx.Message.Parts[0].(a2a.TextPart); ok && t.Text == "ask" {
		state, reply = a2a.TaskStateInputRequired, "Which colour?\nWhite or green."
	}
	ev := a2a.NewStatusUpdateEvent(reqCtx, state, a2a.NewMessageForTask(a2a.MessageRoleAgent, reqCtx, a2a.TextPart{Text: reply}))
	ev.Final = true
	return q.Write(ctx, ev)
}

func (answering) Cancel(context.Context, *a2asrv.RequestContext, eventqueue.Queue) error { return nil }

// peerTool is tools/a2a's tool, as dax-a2a builds it, over a remote
// agent in httptest.
func peerTool(t *testing.T) agenttool.Tool {
	t.Helper()
	srv := httptest.NewServer(a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(answering{})))
	t.Cleanup(srv.Close)
	card := &a2a.AgentCard{Name: "Painter", URL: srv.URL, PreferredTransport: a2a.TransportProtocolJSONRPC, Capabilities: a2a.AgentCapabilities{Streaming: true}}
	client, err := a2aclient.NewFromCard(context.Background(), card)
	if err != nil {
		t.Fatal(err)
	}
	return a2atool.New(client, card, a2atool.WithName("painter"))
}

// A peer's call shows its request, the start of the answer, and the
// question of a call that ended asking for more input, from what
// tools/a2a wrote.
func TestAPeersCallShowsItsRequestAndAnswer(t *testing.T) {
	rs := Peers("painter", "reviewer")
	if rs["painter"] == nil || rs["reviewer"] == nil || len(rs) != 2 {
		t.Fatalf("renderers %v", rs)
	}
	r := rs["painter"]
	tl := peerTool(t)

	c := run(t, tl, `{"input":"Paint the fence.\nUse white."}`)
	if got, want := head(r, c), "Paint the fence."; got != want {
		t.Errorf("head %q, want %q", got, want)
	}
	if got, want := drawn(r.Body(c, false)), []string{"Painted the fence.", "Two coats, white."}; !slices.Equal(got, want) {
		t.Errorf("answer %q, want %q", got, want)
	}
	running := c
	running.State, running.Output = view.CallRunning, ""
	if got := drawn(r.Body(running, false)); got != nil {
		t.Errorf("a running call has a body: %q", got)
	}

	asked := run(t, tl, `{"input":"ask"}`)
	if got, want := drawn(r.Body(asked, false)), []string{"asks: Which colour?", "… 1 more line (ctrl+o)"}; !slices.Equal(got, want) {
		t.Errorf("collapsed question %q, want %q (output %q)", got, want, asked.Output)
	}
	if got, want := drawn(r.Body(asked, true)), []string{"asks: Which colour?", "White or green."}; !slices.Equal(got, want) {
		t.Errorf("expanded question %q, want %q", got, want)
	}

	failed := view.Call{Name: "painter", Args: `{"input":"x"}`, State: view.CallEnded, Output: "Error: a2a: connection refused"}
	if got, want := drawn(r.Body(failed, false)), []string{"a2a: connection refused"}; !slices.Equal(got, want) {
		t.Errorf("failure %q, want %q", got, want)
	}
	if got := head(r, view.Call{Name: "painter", Args: `{"query":"x"}`}); got != "(declined)" {
		t.Errorf("a call of another shape: %q", got)
	}
}
