package a2a_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	a2atool "github.com/ChristopherDavenport/agentturn/tools/a2a"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/a2aproject/a2a-go/a2asrv/eventqueue"

	"github.com/ChristopherDavenport/dax/agent"
	daxa2a "github.com/ChristopherDavenport/dax/ext/a2a"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/policy"
)

func TestARemoteIsChecked(t *testing.T) {
	ok := daxa2a.Remote{Name: "reviewer", URL: "https://reviewer.example"}
	for _, tc := range []struct {
		name string
		edit func(*daxa2a.Remote)
		want string // "" passes
	}{
		{"https", func(*daxa2a.Remote) {}, ""},
		{"a path", func(r *daxa2a.Remote) { r.URL = "https://agents.example/reviewer" }, ""},
		{"http to localhost", func(r *daxa2a.Remote) { r.URL = "http://localhost:8080" }, ""},
		{"http to 127.0.0.1", func(r *daxa2a.Remote) { r.URL = "http://127.0.0.1:8080" }, ""},
		{"http to ::1", func(r *daxa2a.Remote) { r.URL = "http://[::1]:8080" }, ""},
		{"a peer with a token", func(r *daxa2a.Remote) { r.Relation, r.TokenEnv = "peer", "REVIEWER_TOKEN" }, ""},
		{"a subsession", func(r *daxa2a.Remote) { r.Relation = "subsession" }, ""},
		{"http elsewhere", func(r *daxa2a.Remote) { r.URL = "http://reviewer.example" }, "loopback"},
		{"another scheme", func(r *daxa2a.Remote) { r.URL = "ftp://reviewer.example" }, "https"},
		{"no host", func(r *daxa2a.Remote) { r.URL = "https:///x" }, "https"},
		{"credentials in the URL", func(r *daxa2a.Remote) { r.URL = "https://me:secret@reviewer.example" }, "token_env"},
		{"a query", func(r *daxa2a.Remote) { r.URL = "https://reviewer.example/?key=secret" }, "no query"},
		{"an empty name", func(r *daxa2a.Remote) { r.Name = "" }, "peer name"},
		{"a name with a space", func(r *daxa2a.Remote) { r.Name = "code review" }, "peer name"},
		{"a name starting with a digit", func(r *daxa2a.Remote) { r.Name = "1st" }, "peer name"},
		{"an MCP name", func(r *daxa2a.Remote) { r.Name = "mcp__x" }, "mcp__"},
		{"an unknown relation", func(r *daxa2a.Remote) { r.Relation = "child" }, "relation"},
		{"a token that is not a variable", func(r *daxa2a.Remote) { r.TokenEnv = "$TOKEN" }, "token_env"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := ok
			tc.edit(&r)
			err := r.Check()
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("Check() = %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("Check() = %v, want an error about %q", err, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Errorf("the error quotes a credential: %v", err)
			}
		})
	}
}

func TestTheExtension(t *testing.T) {
	none := daxa2a.New(daxa2a.Options{})
	if none.Name != "dax-a2a" || none.Owns != nil || none.Kit != nil || none.Tools != nil || none.Renderers != nil {
		t.Errorf("with no peers: %+v", none)
	}
	e := daxa2a.New(daxa2a.Options{Remotes: []daxa2a.Remote{{Name: "a", URL: "https://a.example"}, {Name: "b", URL: "https://b.example"}}})
	if !slices.Equal(e.Owns, []string{"a", "b"}) || e.Tools != nil || e.Kit == nil {
		t.Errorf("with peers: %+v", e)
	}
	if p := e.Policy; len(p.Allow)+len(p.Ask)+len(p.Deny) != 0 {
		t.Errorf("ships rules %+v; a peer's call asks by default", p)
	}
	if r := e.Renderers(""); r["a"] == nil || r["b"] == nil {
		t.Errorf("renderers %v", r)
	}
}

// remote is an A2A agent in httptest: its card at the well-known path,
// JSON-RPC at the root, and the Authorization header of every request
// kept.
type remote struct {
	*httptest.Server
	mu   sync.Mutex
	auth []string
}

func serve(t *testing.T, exec a2asrv.AgentExecutor) *remote {
	t.Helper()
	r := &remote{}
	card := &a2a.AgentCard{
		// A name unlike the tool's: the configured name is the tool's.
		Name: "Card Name", Description: "Paints things.",
		Capabilities: a2a.AgentCapabilities{Streaming: true}, PreferredTransport: a2a.TransportProtocolJSONRPC,
	}
	rpc := a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(exec))
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.auth = append(r.auth, req.Header.Get("Authorization"))
		r.mu.Unlock()
		if req.URL.Path == a2asrv.WellKnownAgentCardPath {
			c := *card
			c.URL = r.URL
			_ = json.NewEncoder(w).Encode(&c)
			return
		}
		rpc.ServeHTTP(w, req)
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *remote) authorizations() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.auth)
}

// echoing completes each task with its request.
type echoing struct{}

func (echoing) Execute(ctx context.Context, reqCtx *a2asrv.RequestContext, q eventqueue.Queue) error {
	ev := a2a.NewStatusUpdateEvent(reqCtx, a2a.TaskStateCompleted, a2a.NewMessageForTask(a2a.MessageRoleAgent, reqCtx, a2a.TextPart{Text: "echo: " + text(reqCtx.Message)}))
	ev.Final = true
	return q.Write(ctx, ev)
}

func (echoing) Cancel(context.Context, *a2asrv.RequestContext, eventqueue.Queue) error { return nil }

// stalling works until it is cancelled or released, and takes as long
// over the cancel as the test lets it: it answers a cancel only once
// released.
type stalling struct {
	started   chan a2a.TaskID
	cancelled chan a2a.TaskID
	release   chan struct{}
}

// newStalling is a stalling agent served; it is released before the
// server closes.
func newStalling(t *testing.T) (*stalling, *remote) {
	s := &stalling{started: make(chan a2a.TaskID, 4), cancelled: make(chan a2a.TaskID, 4), release: make(chan struct{})}
	r := serve(t, s)
	t.Cleanup(func() { close(s.release) })
	return s, r
}

func (s *stalling) Execute(ctx context.Context, reqCtx *a2asrv.RequestContext, q eventqueue.Queue) error {
	if err := q.Write(ctx, a2a.NewStatusUpdateEvent(reqCtx, a2a.TaskStateWorking, nil)); err != nil {
		return err
	}
	s.started <- reqCtx.TaskID
	select {
	case <-s.release:
	case <-time.After(10 * time.Second):
	}
	return nil
}

func (s *stalling) Cancel(ctx context.Context, reqCtx *a2asrv.RequestContext, q eventqueue.Queue) error {
	s.cancelled <- reqCtx.TaskID
	<-s.release
	ev := a2a.NewStatusUpdateEvent(reqCtx, a2a.TaskStateCanceled, nil)
	ev.Final = true
	return q.Write(ctx, ev)
}

// asking asks which colour on a task's first message, and paints with
// the answer when it comes on the same task.
type asking struct{ cancelled chan a2a.TaskID }

func (asking) Execute(ctx context.Context, reqCtx *a2asrv.RequestContext, q eventqueue.Queue) error {
	if reqCtx.StoredTask == nil {
		ev := a2a.NewStatusUpdateEvent(reqCtx, a2a.TaskStateInputRequired, a2a.NewMessageForTask(a2a.MessageRoleAgent, reqCtx, a2a.TextPart{Text: "which colour?"}))
		ev.Final = true
		return q.Write(ctx, ev)
	}
	ev := a2a.NewStatusUpdateEvent(reqCtx, a2a.TaskStateCompleted, a2a.NewMessageForTask(a2a.MessageRoleAgent, reqCtx, a2a.TextPart{Text: "painted " + text(reqCtx.Message)}))
	ev.Final = true
	return q.Write(ctx, ev)
}

func (a asking) Cancel(ctx context.Context, reqCtx *a2asrv.RequestContext, q eventqueue.Queue) error {
	a.cancelled <- reqCtx.TaskID
	ev := a2a.NewStatusUpdateEvent(reqCtx, a2a.TaskStateCanceled, nil)
	ev.Final = true
	return q.Write(ctx, ev)
}

func text(m *a2a.Message) string {
	var b strings.Builder
	for _, p := range m.Parts {
		if t, ok := p.(a2a.TextPart); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// scripted makes the calls in order, one per model call, then answers.
type scripted struct{ calls [][2]string }

func (m *scripted) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	step := 0
	for _, it := range req.Input {
		if _, ok := it.(*openresponses.FunctionCallOutput); ok {
			step++
		}
	}
	em := openresponses.NewEmitter(sink, openresponses.NewResponse(req))
	if step < len(m.calls) {
		call, err := em.FunctionCall("", m.calls[step][0])
		if err != nil {
			return err
		}
		if err := call.Arguments(m.calls[step][1]); err != nil {
			return err
		}
		if err := call.Close(); err != nil {
			return err
		}
		return em.Complete()
	}
	msg, err := em.Message(openresponses.PhaseFinalAnswer)
	if err != nil {
		return err
	}
	if err := msg.Text("done"); err != nil {
		return err
	}
	if err := msg.Close(); err != nil {
		return err
	}
	return em.Complete()
}

// logged collects the session's notes.
type logged struct {
	mu    sync.Mutex
	notes []string
}

func (l *logged) log(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.notes = append(l.notes, fmt.Sprintf(format, args...))
}

func (l *logged) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.notes, "\n")
}

// open is a session of dax-a2a over remotes, whose model calls the
// first remote once with input, under the policy's default of asking
// and the user's allow rules.
func open(t *testing.T, remotes []daxa2a.Remote, allow []string, input string) (*agent.Session, *logged) {
	t.Helper()
	var log logged
	model := &scripted{}
	if len(remotes) > 0 {
		args, _ := json.Marshal(map[string]string{"input": input})
		model.calls = [][2]string{{remotes[0].Name, string(args)}}
	}
	o := agent.Options{
		Model: "scripted", Streamer: model,
		Dir: t.TempDir(), UserDir: t.TempDir(),
		Store:      agentsession.NewMemoryStore(),
		Policy:     &policy.Settings{Builtin: true, Fallback: "ask", User: policy.Rules{Allow: allow}},
		Extensions: []extension.Extension{daxa2a.New(daxa2a.Options{Remotes: remotes, CardTimeout: 2 * time.Second})},
		Log:        log.log,
	}
	s, err := agent.New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, &log
}

func offered(s *agent.Session) []string {
	var names []string
	for _, tl := range s.Tools() {
		names = append(names, tl.Name)
	}
	return names
}

// output is the output the call to name got in the session's record.
func output(t *testing.T, s *agent.Session) string {
	t.Helper()
	for _, it := range s.Agent().State().Transcript {
		if o, ok := it.(*openresponses.FunctionCallOutput); ok {
			return o.Output.Text
		}
	}
	t.Fatal("no call output in the transcript")
	return ""
}

// callees are the Callee records the session holds, which agentturn's
// recorder writes beside the call.
func callees(t *testing.T, s *agent.Session) []a2atool.Callee {
	t.Helper()
	rd := s.Recorder().Store().(agentsession.Reader)
	sess, err := rd.Read(t.Context(), s.ID())
	if err != nil {
		t.Fatal(err)
	}
	var out []a2atool.Callee
	for _, e := range sess.Entries() {
		if c, ok := e.(*agentsession.CustomEntry); ok && c.NS == a2atool.CalleeNS {
			var cl a2atool.Callee
			if err := json.Unmarshal(c.Data, &cl); err != nil {
				t.Fatal(err)
			}
			out = append(out, cl)
		}
	}
	return out
}

// Each peer whose card is fetched is one tool, by the configured name;
// one whose card cannot be fetched, or whose token is not set, is not
// offered, and the session says why once, never with the token.
func TestEachPeerIsOfferedOrSaysWhyNot(t *testing.T) {
	up := serve(t, echoing{})
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	t.Setenv("PEER_TOKEN", "s3cret-token")
	t.Setenv("UNSET_TOKEN", "")
	s, log := open(t, []daxa2a.Remote{
		{Name: "painter", URL: up.URL},
		{Name: "gone", URL: down.URL},
		{Name: "keyless", URL: up.URL, TokenEnv: "UNSET_TOKEN"},
		{Name: "keyed", URL: up.URL, TokenEnv: "PEER_TOKEN", Relation: daxa2a.Peer},
	}, nil, "")
	names := offered(s)
	for _, want := range []string{"painter", "keyed"} {
		if !slices.Contains(names, want) {
			t.Errorf("tools %v lack %s", names, want)
		}
	}
	for _, not := range []string{"gone", "keyless", "card_name"} {
		if slices.Contains(names, not) {
			t.Errorf("tools %v offer %s", names, not)
		}
	}
	notes := log.String()
	if strings.Count(notes, "peer gone not offered") != 1 || strings.Count(notes, "peer keyless not offered: $UNSET_TOKEN") != 1 {
		t.Errorf("notes %q, want each peer not offered said once", notes)
	}
	if strings.Contains(notes, "s3cret") {
		t.Errorf("the notes carry the token: %q", notes)
	}
}

// A card that cannot be fetched in time does not hold the session up.
func TestASlowCardIsNotWaitedOn(t *testing.T) {
	block := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(slow.Close)
	t.Cleanup(func() { close(block) })
	start := time.Now()
	ext := daxa2a.New(daxa2a.Options{Remotes: []daxa2a.Remote{{Name: "slow", URL: slow.URL}}, CardTimeout: 200 * time.Millisecond})
	var log logged
	s, err := agent.New(context.Background(), agent.Options{
		Model: "scripted", Streamer: &scripted{}, Dir: t.TempDir(), UserDir: t.TempDir(),
		Store: agentsession.NewMemoryStore(), Extensions: []extension.Extension{ext}, Log: log.log,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("the session waited %v on a card", d)
	}
	if slices.Contains(offered(s), "slow") || !strings.Contains(log.String(), "peer slow not offered") {
		t.Errorf("tools %v, notes %q", offered(s), log.String())
	}
}

// A call the user's rules do not allow asks, as any extension's tool's;
// one they allow runs, carrying the token to the configured origin
// only, and the record names the callee by the URL called.
func TestACallAsksThenCompletes(t *testing.T) {
	up := serve(t, echoing{})
	t.Setenv("PEER_TOKEN", "s3cret-token")
	remotes := []daxa2a.Remote{{Name: "painter", URL: up.URL, TokenEnv: "PEER_TOKEN"}}

	t.Run("asks by default", func(t *testing.T) {
		s, _ := open(t, remotes, nil, "paint the fence")
		var asked []string
		end, err := agent.Drive(t.Context(), s, agent.Rules{Permit: func(c *openresponses.FunctionCall, _ string) (bool, string) {
			asked = append(asked, c.Name)
			return false, ""
		}}, openresponses.UserText("go"))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(asked, []string{"painter"}) || end.Reason != agentturn.ReasonDone {
			t.Errorf("asked about %v, ended %s", asked, end.Reason)
		}
	})

	t.Run("allowed, it completes", func(t *testing.T) {
		s, log := open(t, remotes, []string{"painter"}, "paint the fence")
		if _, err := agent.Drive(t.Context(), s, agent.Rules{}, openresponses.UserText("go")); err != nil {
			t.Fatal(err)
		}
		if got := output(t, s); got != "echo: paint the fence" {
			t.Errorf("output %q", got)
		}
		for _, a := range up.authorizations() {
			if a != "Bearer s3cret-token" {
				t.Errorf("a request carried Authorization %q", a)
			}
		}
		cs := callees(t, s)
		if len(cs) != 1 || cs[0].URL != up.URL || cs[0].Relation != daxa2a.Subsession || cs[0].Task == "" {
			t.Errorf("callees %+v, want the task at %s as a subsession", cs, up.URL)
		}
		rd := s.Recorder().Store().(agentsession.Reader)
		sess, err := rd.Read(t.Context(), s.ID())
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range sess.Entries() {
			if b, _ := json.Marshal(e); strings.Contains(string(b), "s3cret") {
				t.Errorf("the record carries the token: %s", b)
			}
		}
		if strings.Contains(log.String(), "s3cret") {
			t.Errorf("the notes carry the token")
		}
	})
}

// A token goes to the configured origin alone: a card that names an
// endpoint elsewhere gets none, and one over http to another host is
// not called at all.
func TestTheTokenStaysWithTheConfiguredOrigin(t *testing.T) {
	other := serve(t, echoing{})
	t.Setenv("PEER_TOKEN", "s3cret-token")
	for _, tc := range []struct {
		name, endpoint string
		reached        bool
	}{
		{"another loopback origin", other.URL, true},
		{"http to another host", "http://peer.invalid", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(other.authorizations())
			card := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(&a2a.AgentCard{Name: "x", URL: tc.endpoint, PreferredTransport: a2a.TransportProtocolJSONRPC})
			}))
			t.Cleanup(card.Close)
			s, _ := open(t, []daxa2a.Remote{{Name: "painter", URL: card.URL, TokenEnv: "PEER_TOKEN"}}, []string{"painter"}, "paint")
			if _, err := agent.Drive(t.Context(), s, agent.Rules{}, openresponses.UserText("go")); err != nil {
				t.Fatal(err)
			}
			got := other.authorizations()[before:]
			if tc.reached != (len(got) > 0) {
				t.Errorf("requests at the endpoint %v, want reached=%v", got, tc.reached)
			}
			for _, a := range got {
				if a != "" {
					t.Errorf("the endpoint got Authorization %q", a)
				}
			}
			if !tc.reached && !strings.Contains(output(t, s), "loopback") {
				t.Errorf("output %q, want the endpoint refused", output(t, s))
			}
		})
	}
}

// An abort gives a subsession's task up: the remote agent is sent
// tasks/cancel, and the run ends without waiting for it to answer. A
// peer's task is never cancelled.
func TestAnAbortCancelsASubsessionsTaskOnly(t *testing.T) {
	for _, rel := range []string{daxa2a.Subsession, daxa2a.Peer} {
		t.Run(rel, func(t *testing.T) {
			agentSide, up := newStalling(t)
			s, _ := open(t, []daxa2a.Remote{{Name: "painter", URL: up.URL, Relation: rel}}, []string{"painter"}, "paint slowly")
			done := make(chan *agentturn.RunEnd, 1)
			go func() {
				end, _ := agent.Drive(context.Background(), s, agent.Rules{}, openresponses.UserText("go"))
				done <- end
			}()
			var task a2a.TaskID
			select {
			case task = <-agentSide.started:
			case <-time.After(5 * time.Second):
				t.Fatal("the remote agent never started the task")
			}
			// The call knows its task once the record names it.
			for deadline := time.Now().Add(5 * time.Second); len(callees(t, s)) == 0; time.Sleep(10 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("the call never recorded its callee")
				}
			}
			if cs := callees(t, s); cs[0].Relation != rel || string(task) != cs[0].Task {
				t.Errorf("callee %+v, want task %s as a %s", cs[0], task, rel)
			}
			s.Abort()
			select {
			case end := <-done:
				if end == nil || end.Reason != agentturn.ReasonAborted {
					t.Errorf("run ended %+v, want aborted", end)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("the abort waited on the remote agent")
			}
			if rel == daxa2a.Peer {
				select {
				case id := <-agentSide.cancelled:
					t.Errorf("a peer's task %s was cancelled", id)
				case <-time.After(300 * time.Millisecond):
				}
				return
			}
			select {
			case id := <-agentSide.cancelled:
				if id != task {
					t.Errorf("cancelled %s, want the call's task %s", id, task)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the remote agent saw no tasks/cancel")
			}
		})
	}
}

// A remote agent's question reaches whoever answers the session's
// questions, and the answer goes on the same task; a refusal ends the
// call and gives the task up; with nobody answering, the call ends with
// the question for the model and nothing answers in a person's place.
func TestTheRemotesQuestionGoesToTheSession(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reply     func(agentturn.Question) agenttool.Answer
		want      string
		cancelled bool
	}{
		{"answered", func(agentturn.Question) agenttool.Answer {
			return agenttool.Answer{Action: agenttool.ActionAccept, Note: "blue"}
		}, "painted blue", false},
		{"a bare yes", func(agentturn.Question) agenttool.Answer {
			return agenttool.Answer{Action: agenttool.ActionAccept}
		}, "painted yes", false},
		{"declined", func(agentturn.Question) agenttool.Answer {
			return agenttool.Answer{Action: agenttool.ActionDecline, Note: "pick one yourself"}
		}, `painter asked "which colour?" and the question was declined; the user said: pick one yourself`, true},
		{"nobody answering", nil, "the agent needs more input: which colour?", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agentSide := asking{cancelled: make(chan a2a.TaskID, 4)}
			up := serve(t, agentSide)
			s, _ := open(t, []daxa2a.Remote{{Name: "painter", URL: up.URL}}, []string{"painter"}, "paint the fence")
			var asked []string
			rules := agent.Rules{}
			if tc.reply != nil {
				rules.Reply = func(q agentturn.Question) agenttool.Answer {
					asked = append(asked, q.Elicitation.Message)
					return tc.reply(q)
				}
			}
			if _, err := agent.Drive(t.Context(), s, rules, openresponses.UserText("go")); err != nil {
				t.Fatal(err)
			}
			if got := output(t, s); !strings.Contains(got, tc.want) {
				t.Errorf("output %q, want %q", got, tc.want)
			}
			if tc.reply != nil && (len(asked) != 1 || !strings.Contains(asked[0], "painter (an A2A agent) asks: which colour?")) {
				t.Errorf("asked %q", asked)
			}
			select {
			case id := <-agentSide.cancelled:
				if !tc.cancelled {
					t.Errorf("task %s cancelled", id)
				}
			case <-time.After(300 * time.Millisecond):
				if tc.cancelled {
					t.Error("the declined task was not given up")
				}
			}
			if cs := callees(t, s); len(cs) != 1 {
				t.Errorf("callees %+v, want one task for the call and its answer", cs)
			}
		})
	}
}
