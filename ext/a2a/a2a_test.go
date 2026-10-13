package a2a_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
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
	"github.com/ChristopherDavenport/agentturn/session"
	a2atool "github.com/ChristopherDavenport/agentturn/tools/a2a"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/a2aproject/a2a-go/a2asrv/eventqueue"

	"github.com/ChristopherDavenport/dax/agent"
	daxa2a "github.com/ChristopherDavenport/dax/ext/a2a"
	"github.com/ChristopherDavenport/dax/ext/coding"
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
	return serveWith(t, exec, serveOpts{})
}

// serveOpts change what serve serves.
type serveOpts struct {
	// endpoint is the URL the card names, given the server's own; the
	// server's own when nil.
	endpoint func(self string) string
	// route answers a request before the card and the agent do, when it
	// returns true.
	route func(w http.ResponseWriter, req *http.Request) bool
	// tls serves https.
	tls bool
}

func serveWith(t *testing.T, exec a2asrv.AgentExecutor, o serveOpts) *remote {
	t.Helper()
	r := &remote{}
	card := &a2a.AgentCard{
		// A name unlike the tool's: the configured name is the tool's.
		Name: "Card Name", Description: "Paints things.",
		Capabilities: a2a.AgentCapabilities{Streaming: true}, PreferredTransport: a2a.TransportProtocolJSONRPC,
	}
	rpc := a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(exec))
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.auth = append(r.auth, req.Header.Get("Authorization"))
		r.mu.Unlock()
		if o.route != nil && o.route(w, req) {
			return
		}
		if req.URL.Path == a2asrv.WellKnownAgentCardPath {
			c := *card
			c.URL = r.URL
			if o.endpoint != nil {
				c.URL = o.endpoint(r.URL)
			}
			_ = json.NewEncoder(w).Encode(&c)
			return
		}
		rpc.ServeHTTP(w, req)
	})
	if o.tls {
		r.Server = httptest.NewTLSServer(h)
		tlsTransport = r.Client().Transport
		t.Cleanup(func() { tlsTransport = nil })
	} else {
		r.Server = httptest.NewServer(h)
	}
	t.Cleanup(r.Close)
	return r
}

// tlsTransport trusts the https test server serveWith last started,
// for open to give dax-a2a.
var tlsTransport http.RoundTripper

// requests is how many requests the server has had.
func (r *remote) requests() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.auth)
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
		Extensions: []extension.Extension{daxa2a.New(daxa2a.Options{Remotes: remotes, CardTimeout: 2 * time.Second}.WithTransport(tlsTransport))},
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

// callees are the callees the session's record names, the links
// agentturn's recorder writes for the call's Callee records (a cancel
// notice names no new one).
func callees(t *testing.T, s *agent.Session) []a2atool.Callee {
	t.Helper()
	rd := s.Recorder().Store().(agentsession.Reader)
	sess, err := rd.Read(t.Context(), s.ID())
	if err != nil {
		t.Fatal(err)
	}
	var out []a2atool.Callee
	for _, e := range sess.Entries() {
		if l, ok := e.(*agentsession.LinkEntry); ok && l.URL != "" && l.Cancel == "" {
			out = append(out, a2atool.Callee{URL: l.URL, Task: l.Task, Context: l.Context, Relation: l.Rel})
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

// The card, not the user, names the endpoint, so a card that sends the
// work to another origin (another port here, a host over http, this
// machine for an https peer) is not offered, and nothing is sent there.
func TestACardNamingAnotherOriginIsNotOffered(t *testing.T) {
	other := serve(t, echoing{})
	t.Setenv("PEER_TOKEN", "s3cret-token")
	for _, tc := range []struct {
		name string
		o    serveOpts
	}{
		{"another port on this machine", serveOpts{endpoint: func(string) string { return other.URL }}},
		{"http to another host", serveOpts{endpoint: func(string) string { return "http://peer.invalid" }}},
		{"this machine, named by an https peer", serveOpts{tls: true, endpoint: func(string) string { return other.URL }}},
		{"another scheme at the same host and port", serveOpts{tls: true, endpoint: func(self string) string { return strings.Replace(self, "https:", "http:", 1) }}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := other.requests()
			card := serveWith(t, echoing{}, tc.o)
			s, log := open(t, []daxa2a.Remote{{Name: "painter", URL: card.URL, TokenEnv: "PEER_TOKEN"}}, []string{"painter"}, "paint")
			if slices.Contains(offered(s), "painter") {
				t.Errorf("tools %v offer a peer whose card names another origin", offered(s))
			}
			if n := strings.Count(log.String(), "peer painter not offered: its agent card names the endpoint"); n != 1 {
				t.Errorf("notes %q, want the endpoint named once", log.String())
			}
			if other.requests() != before {
				t.Error("a request reached the other origin")
			}
		})
	}
}

// A redirect to another origin is not followed, on the card's fetch or
// on a call, since Go's client would carry the token to a subdomain, to
// another port, or from https to http; one within the origin is.
func TestARedirectToAnotherOriginIsNotFollowed(t *testing.T) {
	other := serve(t, echoing{})
	t.Setenv("PEER_TOKEN", "s3cret-token")
	away := func(path string) func(http.ResponseWriter, *http.Request) bool {
		return func(w http.ResponseWriter, req *http.Request) bool {
			if req.URL.Path != path || req.URL.Query().Get("moved") != "" {
				return false
			}
			http.Redirect(w, req, other.URL+path, http.StatusTemporaryRedirect)
			return true
		}
	}

	t.Run("the card", func(t *testing.T) {
		before := other.requests()
		card := serveWith(t, echoing{}, serveOpts{route: away(a2asrv.WellKnownAgentCardPath)})
		s, log := open(t, []daxa2a.Remote{{Name: "painter", URL: card.URL, TokenEnv: "PEER_TOKEN"}}, nil, "")
		if slices.Contains(offered(s), "painter") || !strings.Contains(log.String(), "another origin") {
			t.Errorf("tools %v, notes %q", offered(s), log.String())
		}
		if other.requests() != before {
			t.Error("the card's fetch followed the redirect")
		}
	})

	t.Run("a call", func(t *testing.T) {
		before := other.requests()
		up := serveWith(t, echoing{}, serveOpts{route: away("/")})
		s, _ := open(t, []daxa2a.Remote{{Name: "painter", URL: up.URL, TokenEnv: "PEER_TOKEN"}}, []string{"painter"}, "paint")
		if _, err := agent.Drive(t.Context(), s, agent.Rules{}, openresponses.UserText("go")); err != nil {
			t.Fatal(err)
		}
		if got := output(t, s); !strings.Contains(got, "another origin") {
			t.Errorf("output %q, want the redirect refused", got)
		}
		if other.requests() != before {
			t.Error("the call followed the redirect")
		}
		noToken(t, s)
	})

	t.Run("within the origin", func(t *testing.T) {
		var up *remote
		up = serveWith(t, echoing{}, serveOpts{route: func(w http.ResponseWriter, req *http.Request) bool {
			if req.URL.Path != a2asrv.WellKnownAgentCardPath || req.URL.Query().Get("moved") != "" {
				return false
			}
			http.Redirect(w, req, up.URL+a2asrv.WellKnownAgentCardPath+"?moved=1", http.StatusTemporaryRedirect)
			return true
		}})
		s, log := open(t, []daxa2a.Remote{{Name: "painter", URL: up.URL, TokenEnv: "PEER_TOKEN"}}, nil, "")
		if !slices.Contains(offered(s), "painter") {
			t.Errorf("tools %v, notes %q: a redirect within the origin was refused", offered(s), log.String())
		}
	})
}

// noToken fails when the session's record carries the token.
func noToken(t *testing.T, s *agent.Session) {
	t.Helper()
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
// questions, as a question for text, and the answer goes on the same
// task; a refusal ends the call and gives the task up; with nobody
// answering, the call ends with the question for the model, nothing
// answers in a person's place, and the subsession's task is given up. The record holds the question, the
// answer and what was said with a refusal, and never the token.
func TestTheRemotesQuestionGoesToTheSession(t *testing.T) {
	t.Setenv("PEER_TOKEN", "s3cret-token")
	for _, tc := range []struct {
		name      string
		reply     func(agentturn.Question) agenttool.Answer
		want      string
		action    string // the recorded answer's
		content   string
		note      string
		cancelled bool
	}{
		{"answered with a note, as the terminal client does", func(agentturn.Question) agenttool.Answer {
			return agenttool.Answer{Action: agenttool.ActionAccept, Note: "blue"}
		}, "painted blue\n\npainter asked \"which colour?\" and the user answered \"blue\".", "accept", `{"answer":"blue"}`, "", false},
		{"answered with the form, as the REPL does", func(agentturn.Question) agenttool.Answer {
			return agenttool.Answer{Action: agenttool.ActionAccept, Content: json.RawMessage(`{"answer":"green"}`)}
		}, "painted green\n\npainter asked \"which colour?\" and the user answered \"green\".", "accept", `{"answer":"green"}`, "", false},
		{"a bare yes", func(agentturn.Question) agenttool.Answer {
			return agenttool.Answer{Action: agenttool.ActionAccept}
		}, "painted yes\n\npainter asked \"which colour?\" and the user answered \"yes\".", "accept", `{"answer":"yes"}`, "", false},
		{"declined", func(agentturn.Question) agenttool.Answer {
			return agenttool.Answer{Action: agenttool.ActionDecline, Note: "pick one yourself"}
		}, `painter asked "which colour?" and the question was declined`, "decline", "", "pick one yourself", true},
		{"nobody answering", nil, "the agent needs more input: which colour?", "", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agentSide := asking{cancelled: make(chan a2a.TaskID, 4)}
			up := serve(t, agentSide)
			s, _ := open(t, []daxa2a.Remote{{Name: "painter", URL: up.URL, TokenEnv: "PEER_TOKEN"}}, []string{"painter"}, "paint the fence")
			var asked []agentturn.Question
			rules := agent.Rules{}
			if tc.reply != nil {
				rules.Reply = func(q agentturn.Question) agenttool.Answer {
					asked = append(asked, q)
					return tc.reply(q)
				}
			}
			if _, err := agent.Drive(t.Context(), s, rules, openresponses.UserText("go")); err != nil {
				t.Fatal(err)
			}
			got := output(t, s)
			if !strings.Contains(got, tc.want) {
				t.Errorf("output %q, want %q", got, tc.want)
			}
			if tc.note != "" && strings.Contains(got, tc.note) {
				t.Errorf("output %q repeats the person's note, which reaches the model as theirs", got)
			}
			if tc.reply != nil {
				want := `a tool asks: the A2A agent painter (` + up.URL + `) needs an answer to go on: reply with it, or refuse. It says: "which colour?"`
				if len(asked) != 1 || asked[0].Elicitation.Message != want {
					t.Errorf("asked %+v, want %q", asked, want)
				} else if !extension.IsTextQuestion(asked[0].Elicitation) {
					t.Errorf("the question does not ask for text: %s", asked[0].Elicitation.Schema)
				}
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
			if tc.action != "" {
				q, a := recordedQuestion(t, s)
				if !strings.Contains(q.Message, `It says: "which colour?"`) || a.Action != tc.action || string(a.Content) != tc.content || a.Note != tc.note {
					t.Errorf("recorded %q; answer %q content %s note %q", q.Message, a.Action, a.Content, a.Note)
				}
			}
			noToken(t, s)
		})
	}
}

// recordedQuestion is the session's recorded question and its answer.
func recordedQuestion(t *testing.T, s *agent.Session) (ask, answer session.Elicitation) {
	t.Helper()
	rd := s.Recorder().Store().(agentsession.Reader)
	sess, err := rd.Read(t.Context(), s.ID())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range sess.Entries() {
		c, ok := e.(*agentsession.CustomEntry)
		if !ok || c.NS != session.ElicitationNS {
			continue
		}
		var q session.Elicitation
		if err := json.Unmarshal(c.Data, &q); err != nil {
			t.Fatal(err)
		}
		switch q.Phase {
		case session.ElicitationAsk:
			ask = q
		case session.ElicitationAnswer:
			answer = q
		}
	}
	return ask, answer
}

// An abort while the remote agent's question waits gives a subsession's
// task up, as an abort of a working task does; a peer's is left.
func TestAnAbortWhileTheQuestionWaits(t *testing.T) {
	t.Setenv("PEER_TOKEN", "s3cret-token")
	for _, rel := range []string{daxa2a.Subsession, daxa2a.Peer} {
		t.Run(rel, func(t *testing.T) {
			agentSide := asking{cancelled: make(chan a2a.TaskID, 4)}
			up := serve(t, agentSide)
			s, _ := open(t, []daxa2a.Remote{{Name: "painter", URL: up.URL, Relation: rel, TokenEnv: "PEER_TOKEN"}}, []string{"painter"}, "paint")
			asked := make(chan struct{}, 1)
			release := make(chan struct{})
			defer close(release)
			done := make(chan *agentturn.RunEnd, 1)
			go func() {
				end, _ := agent.Drive(context.Background(), s, agent.Rules{Reply: func(agentturn.Question) agenttool.Answer {
					asked <- struct{}{}
					<-release
					return agenttool.Answer{Action: agenttool.ActionAccept, Note: "late"}
				}}, openresponses.UserText("go"))
				done <- end
			}()
			select {
			case <-asked:
			case <-time.After(5 * time.Second):
				t.Fatal("the question never came")
			}
			s.Abort()
			select {
			case end := <-done:
				if end == nil || end.Reason != agentturn.ReasonAborted {
					t.Errorf("run ended %+v, want aborted", end)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the abort waited on the question")
			}
			select {
			case id := <-agentSide.cancelled:
				if rel == daxa2a.Peer {
					t.Errorf("a peer's task %s was cancelled", id)
				}
			case <-time.After(time.Second):
				if rel == daxa2a.Subsession {
					t.Error("the remote agent saw no tasks/cancel")
				}
			}
			noToken(t, s)
		})
	}
}

// What the session notes of a peer is cleaned of escape sequences: a
// card's status line is the remote's text.
func TestANoteCarriesNoEscapes(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = bufio.NewReader(c).ReadString('\n')
				_, _ = io.WriteString(c, "HTTP/1.1 503 \x1b]0;pwned\x07\x1b[2Jgone\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			}()
		}
	}()
	s, log := open(t, []daxa2a.Remote{{Name: "painter", URL: "http://" + ln.Addr().String()}}, nil, "")
	notes := log.String()
	if slices.Contains(offered(s), "painter") || !strings.Contains(notes, "peer painter not offered") || !strings.Contains(notes, "gone") {
		t.Fatalf("tools %v, notes %q", offered(s), notes)
	}
	if strings.ContainsAny(notes, "\x1b\x07") {
		t.Errorf("the note carries control characters: %q", notes)
	}
}

// A peer named like another extension's tool fails the session's start,
// saying whose the name is.
func TestAPeerNamedLikeAnotherToolFailsTheStart(t *testing.T) {
	up := serve(t, echoing{})
	_, err := agent.New(context.Background(), agent.Options{
		Model: "scripted", Streamer: &scripted{}, Dir: t.TempDir(), UserDir: t.TempDir(), Store: agentsession.NewMemoryStore(),
		Extensions: []extension.Extension{coding.New(0), daxa2a.New(daxa2a.Options{Remotes: []daxa2a.Remote{{Name: "read", URL: up.URL}}})},
	})
	if err == nil || !strings.Contains(err.Error(), `extension dax-a2a: tool "read": extension dax-coding has that name`) {
		t.Errorf("err = %v", err)
	}
}

// A card is read up to a bound: one past it is not offered.
func TestABigCardIsNotRead(t *testing.T) {
	big := serveWith(t, echoing{}, serveOpts{route: func(w http.ResponseWriter, req *http.Request) bool {
		if req.URL.Path != a2asrv.WellKnownAgentCardPath {
			return false
		}
		_, _ = io.WriteString(w, `{"name":"x","description":"`+strings.Repeat("x", 2<<20)+`"}`)
		return true
	}})
	s, log := open(t, []daxa2a.Remote{{Name: "painter", URL: big.URL}}, nil, "")
	if slices.Contains(offered(s), "painter") || !strings.Contains(log.String(), "the card is over") {
		t.Errorf("tools %v, notes %q", offered(s), log.String())
	}
}
