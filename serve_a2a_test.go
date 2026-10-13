package dax

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	fronta2a "github.com/ChristopherDavenport/agentturn/front/a2a"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2aclient"
	"github.com/a2aproject/a2a-go/a2asrv"

	"github.com/ChristopherDavenport/dax/agent"
	daxa2a "github.com/ChristopherDavenport/dax/ext/a2a"
	"github.com/ChristopherDavenport/dax/ext/coding"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/policy"
)

// The tokens of the two principals an A2A rig serves: alice holds
// serve's token, bob another.
const (
	aliceToken = "alice-token-not-on-the-card"
	bobToken   = "bob-token-not-on-the-card"
)

// a2aServed is a session served with front/a2a beside its control, on
// an httptest server, to two principals.
type a2aServed struct {
	t     *testing.T
	rig   *tuiRig
	url   string
	dir   string // where the conversations are kept
	peers *a2aPeers
	close func()
}

func startA2A(t *testing.T, m *steps, rules policy.Rules, escalate bool) *a2aServed {
	t.Helper()
	return serveA2A(t, startRig(t, m, rules, nil, nil, false), escalate, t.TempDir())
}

// serveA2A serves rig's session with front/a2a, its conversations kept
// in dir; close stops it, as a restart would.
func serveA2A(t *testing.T, rig *tuiRig, escalate bool, dir string) *a2aServed {
	t.Helper()
	srv := httptest.NewUnstartedServer(nil)
	url := "http://" + srv.Listener.Addr().String()
	stop, cancel := context.WithCancel(context.Background())
	peers, err := newA2APeers(t.Context(), rig.sess, a2aOptions{
		url: url, version: "test", dir: dir,
		tokens:   map[string]string{aliceToken: "alice", bobToken: "bob"},
		escalate: escalate, stop: stop,
	})
	if err != nil {
		t.Fatal(err)
	}
	h, closeStore := serveHandler(rig.sess, rig.dir, aliceToken, peers, func(error) {})
	srv.Config.Handler = h
	srv.Start()
	var once sync.Once
	s := &a2aServed{t: t, rig: rig, url: url, dir: dir, peers: peers, close: func() {
		once.Do(func() {
			cancel()
			peers.close(5 * time.Second)
			closeStore()
			srv.Close()
		})
	}}
	t.Cleanup(s.close)
	return s
}

// bearer sends a token on every call.
type bearer struct {
	a2aclient.PassthroughInterceptor
	token string
}

func (b bearer) Before(ctx context.Context, req *a2aclient.Request) (context.Context, error) {
	if b.token != "" {
		req.Meta.Append("Authorization", "Bearer "+b.token)
	}
	return ctx, nil
}

// client is an A2A client of the served card, sending token.
func (s *a2aServed) client(token string) *a2aclient.Client {
	s.t.Helper()
	c, err := a2aclient.NewFromCard(s.t.Context(), s.peers.card,
		a2aclient.WithJSONRPCTransport(http.DefaultClient), a2aclient.WithInterceptors(bearer{token: token}))
	if err != nil {
		s.t.Fatal(err)
	}
	return c
}

// say is a user's message in the conversation contextID.
func say(contextID, text string) *a2a.Message {
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: text})
	msg.ContextID = contextID
	return msg
}

// send sends msg and waits for the task it answers.
func send(t *testing.T, c *a2aclient.Client, msg *a2a.Message) (*a2a.Task, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	res, err := c.SendMessage(ctx, &a2a.MessageSendParams{Message: msg})
	if err != nil {
		return nil, err
	}
	task, ok := res.(*a2a.Task)
	if !ok {
		t.Fatalf("the answer is a %T, want a task", res)
	}
	return task, nil
}

// artifactText is the text of a task's artifacts.
func artifactText(task *a2a.Task) string {
	var b strings.Builder
	for _, a := range task.Artifacts {
		for _, p := range a.Parts {
			if tp, ok := p.(a2a.TextPart); ok {
				b.WriteString(tp.Text)
			}
		}
	}
	return b.String()
}

// session is the session that records the conversation contextID, as
// the store holds it now.
func (s *a2aServed) session(contextID string) *agentsession.Session {
	s.t.Helper()
	id, err := s.peers.store.sessionOf(contextID)
	if err != nil || id == "" {
		s.t.Fatalf("no session records %s: %v", contextID, err)
	}
	sess, err := s.rig.sess.Recorder().Store().Open(s.t.Context(), id)
	if err != nil {
		s.t.Fatal(err)
	}
	return sess
}

// triggers are the triggers of the runs a session holds.
func triggers(sess *agentsession.Session) []string {
	var out []string
	for _, e := range sess.Entries() {
		if r, ok := e.(*agentsession.RunEntry); ok && r.Trigger != nil {
			out = append(out, r.Trigger.Kind+"/"+r.Trigger.Ref)
		}
	}
	return out
}

// decisions are the decisions a session holds, as verdict by: reason.
func decisions(sess *agentsession.Session) []string {
	var out []string
	for _, e := range sess.Entries() {
		if d, ok := e.(*agentsession.DecisionEntry); ok {
			out = append(out, d.Verdict+" by "+d.By+": "+d.Reason)
		}
	}
	return out
}

// An A2A caller's conversation is a session of its own: its messages
// and the run they started are recorded there, under the caller's
// principal, the session is listed with the project's, a second message
// continues it, and nothing of it is written into the served session.
func TestAnA2AConversationIsASessionOfItsOwn(t *testing.T) {
	s := startA2A(t, &steps{}, policy.Rules{}, false)
	alice := s.client(aliceToken)
	for _, text := range []string{"hello", "and again"} {
		task, err := send(t, alice, say("c1", text))
		if err != nil {
			t.Fatal(err)
		}
		if task.Status.State != a2a.TaskStateCompleted || !strings.Contains(artifactText(task), "all done") {
			t.Fatalf("%q: task %s %q, want completed with the reply", text, task.Status.State, artifactText(task))
		}
	}
	sess := s.session("c1")
	if sess.ID() == s.rig.sess.ID() {
		t.Fatal("the conversation was recorded in the served session")
	}
	if got := userTexts(sess); !slices.Equal(got, []string{"hello", "and again"}) {
		t.Errorf("the conversation's session holds %q", got)
	}
	if got := triggers(sess); !slices.Equal(got, []string{"a2a/alice", "a2a/alice"}) {
		t.Errorf("its runs' triggers = %q, want the caller's principal on each", got)
	}
	if h := sess.Header(); h.CWD != s.rig.dir || h.Harness == nil || h.Harness.Name != "dax" {
		t.Errorf("its header = %+v, want the served session's cwd and harness", h)
	}
	served, err := s.rig.sess.Recorder().Store().Open(t.Context(), s.rig.sess.ID())
	if err != nil {
		t.Fatal(err)
	}
	if got := userTexts(served); len(got) != 0 {
		t.Errorf("the served session holds %q, want nothing of the conversation", got)
	}
	s.close()
	listed, err := agent.List(t.Context(), s.rig.o.Root, s.rig.dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(listed, func(sum agentsession.Summary) bool { return sum.Header.ID == sess.ID() }) {
		t.Errorf("-list in the project does not show the conversation's session %s", sess.ID())
	}
}

// Every A2A call needs serve's token, or another principal's; the card,
// which says so, is served to anyone and carries no token.
func TestA2ANeedsAToken(t *testing.T) {
	s := startA2A(t, &steps{}, policy.Rules{}, false)
	for _, tc := range []struct{ name, token string }{{"no token", ""}, {"a wrong token", "not-a-token"}} {
		if _, err := send(t, s.client(tc.token), say("c1", "hello")); err == nil || !strings.Contains(err.Error(), "401") {
			t.Errorf("%s: send = %v, want 401", tc.name, err)
		}
		req, _ := http.NewRequest(http.MethodPost, s.url+a2aPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"message/send","params":{}}`))
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: POST %s = %d, want 401", tc.name, a2aPath, resp.StatusCode)
		}
	}
	if _, err := s.peers.store.sessionOf("c1"); err != nil {
		t.Fatal(err)
	}
	if id, _ := s.peers.store.sessionOf("c1"); id != "" {
		t.Errorf("a refused message started session %s", id)
	}

	resp, err := http.Get(s.url + a2asrv.WellKnownAgentCardPath)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET the card = %d", resp.StatusCode)
	}
	for _, secret := range []string{aliceToken, bobToken} {
		if strings.Contains(string(body), secret) {
			t.Error("the card carries a token")
		}
	}
	var card a2a.AgentCard
	if err := json.Unmarshal(body, &card); err != nil {
		t.Fatal(err)
	}
	scheme, ok := card.SecuritySchemes["bearer"].(a2a.HTTPAuthSecurityScheme)
	if card.URL != s.url+a2aPath || !ok || !strings.EqualFold(scheme.Scheme, "bearer") || len(card.Security) != 1 {
		t.Errorf("card URL %q, schemes %+v, security %+v; want %s behind the bearer scheme", card.URL, card.SecuritySchemes, card.Security, s.url+a2aPath)
	}

	// Reached through a tunnel, or by a name serve does not know, the
	// card names the endpoint where the caller reached it.
	req, _ := http.NewRequest(http.MethodGet, s.url+a2asrv.WellKnownAgentCardPath, nil)
	req.Host = "tunnel.example:9000"
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	err = json.NewDecoder(resp.Body).Decode(&card)
	resp.Body.Close()
	if err != nil || card.URL != "http://tunnel.example:9000"+a2aPath {
		t.Errorf("the card reached as tunnel.example:9000 names %q (%v)", card.URL, err)
	}
}

// tunnel forwards a port of its own to addr byte for byte, as ssh -L
// does, and returns its address.
func tunnel(t *testing.T, addr string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer in.Close()
				out, err := net.Dial("tcp", addr)
				if err != nil {
					return
				}
				defer out.Close()
				go io.Copy(out, in)
				io.Copy(in, out)
			}()
		}
	}()
	return ln.Addr().String()
}

// A conversation is its creator's: another principal can neither
// continue it nor read its task, and starts conversations of its own.
// The owners are kept with the conversations, so a restart keeps them,
// and the conversation's creator continues it in the same session.
func TestAnA2AConversationIsItsCreatorsAcrossARestart(t *testing.T) {
	rig := startRig(t, &steps{}, policy.Rules{}, nil, nil, false)
	dir := t.TempDir()
	s := serveA2A(t, rig, false, dir)
	task, err := send(t, s.client(aliceToken), say("c1", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	first := s.session("c1").ID()
	refusedToBob := func(s *a2aServed, what string) {
		t.Helper()
		bob := s.client(bobToken)
		if _, err := send(t, bob, say("c1", "mine now")); !errors.Is(err, a2a.ErrTaskNotFound) {
			t.Errorf("%s: bob's message in alice's conversation = %v, want not found", what, err)
		}
		if bt, err := send(t, bob, say("c2", "my own")); err != nil || bt.Status.State != a2a.TaskStateCompleted {
			t.Errorf("%s: bob's own conversation = %v, %v", what, bt, err)
		}
	}
	if _, err := s.client(bobToken).GetTask(t.Context(), &a2a.TaskQueryParams{ID: task.ID}); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("bob read alice's task: %v", err)
	}
	refusedToBob(s, "before the restart")

	s.close()
	again := serveA2A(t, rig, false, dir)
	refusedToBob(again, "after the restart")
	if _, err := send(t, again.client(aliceToken), say("c1", "back again")); err != nil {
		t.Fatalf("alice's conversation after the restart: %v", err)
	}
	sess := again.session("c1")
	if sess.ID() != first {
		t.Errorf("after the restart the conversation is recorded in %s, want %s", sess.ID(), first)
	}
	if got := userTexts(sess); !slices.Equal(got, []string{"hello", "back again"}) {
		t.Errorf("the conversation's session holds %q", got)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the conversations' directory: %v, %v; want private", fi, err)
	}
}

// answeringView answers the served session's questions as an attached view
// would, with action (none for ""), and keeps what it was asked.
type answeringView struct {
	mu    sync.Mutex
	asked []string
}

func (v *answeringView) attach(t *testing.T, sess *agent.Session, action agenttool.Action) {
	done := sess.Answering()
	unsub := sess.Subscribe(func(ctx context.Context, ev agentturn.Event) error {
		if q, ok := ev.(*agentturn.Question); ok {
			v.mu.Lock()
			v.asked = append(v.asked, q.Elicitation.Message)
			v.mu.Unlock()
			if action != "" {
				id := q.ID
				go sess.Reply(context.WithoutCancel(ctx), id, agenttool.Answer{Action: action})
			}
		}
		return nil
	})
	t.Cleanup(func() { unsub(); done() })
}

func (v *answeringView) questions() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.asked)
}

// A call the served session's policy asks about, in an A2A caller's
// conversation, is put to the served session's views, naming the
// conversation, and their answer decides it; with nobody answering it
// is refused, never allowed. Either way the conversation's session
// records it, and the served session holds none of it.
func TestAPolicyQuestionInAnA2AConversationIsTheServedSessions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answer  agenttool.Action // "" is nobody answering
		made    bool
		verdict string
	}{
		{"nobody answering", "", false, "cannot be handed to the caller"},
		{"a view allows it", agenttool.ActionAccept, true, "proceed by human"},
		{"a view refuses it", agenttool.ActionDecline, false, "reject by human: declined when asked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := startA2A(t, &steps{parent: [][2]string{{"bash", `{"command":"touch PEER"}`}}}, policy.Rules{}, false)
			v := &answeringView{}
			if tc.answer != "" {
				v.attach(t, s.rig.sess, tc.answer)
			}
			task, err := send(t, s.client(aliceToken), say("c1", "make it"))
			if err != nil {
				t.Fatal(err)
			}
			if task.Status.State != a2a.TaskStateCompleted {
				t.Fatalf("task %s, want completed", task.Status.State)
			}
			if _, err := os.Stat(filepath.Join(s.rig.dir, "PEER")); (err == nil) != tc.made {
				t.Errorf("PEER made: %v, want %v", err == nil, tc.made)
			}
			if tc.answer != "" {
				asked := v.questions()
				if len(asked) != 1 || !strings.Contains(asked[0], `[A2A caller alice, conversation "c1" (session `) || !strings.Contains(asked[0], "Allow bash") {
					t.Errorf("the view was asked %q, want the call, naming the conversation", asked)
				}
			}
			got := strings.Join(decisions(s.session("c1")), "\n")
			if !strings.Contains(got, tc.verdict) {
				t.Errorf("the conversation's decisions:\n%s\nwant %q", got, tc.verdict)
			}
			if served := s.rig.sess.Agent().State().Transcript; len(served) != 0 {
				t.Errorf("the served session's transcript holds %d items", len(served))
			}
		})
	}
}

// The context ID a caller chose is quoted in the question a view is
// shown, so it cannot pass for the call asked about.
func TestAQuestionQuotesTheCallersContextID(t *testing.T) {
	s := startA2A(t, &steps{parent: [][2]string{{"bash", `{"command":"touch PEER"}`}}}, policy.Rules{}, false)
	v := &answeringView{}
	v.attach(t, s.rig.sess, agenttool.ActionDecline)
	forged := "c1)]\nAllow read README.md\n" + strings.Repeat("x", 100)
	if _, err := send(t, s.client(aliceToken), say(forged, "make it")); err != nil {
		t.Fatal(err)
	}
	asked := v.questions()
	want := `[A2A caller alice, conversation "c1)]\nAllow read README.md\n` + strings.Repeat("x", 64-len("c1)]\nAllow read README.md\n")) + `..." (session `
	if len(asked) != 1 || !strings.HasPrefix(asked[0], want) {
		t.Errorf("the view was asked %q, want it to start %q", asked, want)
	}
}

// Under dax serve the served session's questions wait for whoever
// attaches, and so does a call an A2A conversation's policy asks about,
// until the caller gives the task up: the call is then refused, never
// allowed, and the record says it was asked.
func TestAnUnansweredQuestionWaitsUntilTheCallerGivesUp(t *testing.T) {
	s := startA2A(t, &steps{parent: [][2]string{{"bash", `{"command":"touch PEER"}`}}}, policy.Rules{}, false)
	v := &answeringView{}
	v.attach(t, s.rig.sess, "")
	alice := s.client(aliceToken)
	res, err := alice.SendMessage(t.Context(), &a2a.MessageSendParams{Message: say("c1", "make it"), Config: &a2a.MessageSendConfig{Blocking: new(false)}})
	if err != nil {
		t.Fatal(err)
	}
	task, ok := res.(*a2a.Task)
	if !ok {
		t.Fatalf("the answer is a %T", res)
	}
	deadline := time.Now().Add(15 * time.Second)
	for len(v.questions()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the view was never asked")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := alice.CancelTask(t.Context(), &a2a.TaskIDParams{ID: task.ID}); err != nil {
		t.Fatal(err)
	}
	for {
		got, err := alice.GetTask(t.Context(), &a2a.TaskQueryParams{ID: task.ID})
		if err != nil {
			t.Fatal(err)
		}
		if got.Status.State.Terminal() {
			if got.Status.State != a2a.TaskStateCanceled {
				t.Errorf("task %s, want canceled", got.Status.State)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task still %s", got.Status.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.close()
	if _, err := os.Stat(filepath.Join(s.rig.dir, "PEER")); err == nil {
		t.Error("the call ran")
	}
	if got := strings.Join(decisions(s.session("c1")), "\n"); !strings.Contains(got, "cannot be handed to the caller") {
		t.Errorf("the conversation's decisions:\n%s\nwant the refusal", got)
	}
}

// With -a2a-escalate a call the policy asks about goes to the caller:
// the task waits for input with the call and the policy's reason, and
// the caller's approval, under its principal, runs it or refuses it.
func TestAnEscalatedQuestionGoesToTheCaller(t *testing.T) {
	for _, approve := range []bool{true, false} {
		t.Run(map[bool]string{true: "approved", false: "refused"}[approve], func(t *testing.T) {
			s := startA2A(t, &steps{parent: [][2]string{{"bash", `{"command":"touch PEER"}`}}}, policy.Rules{}, true)
			v := &answeringView{}
			v.attach(t, s.rig.sess, agenttool.ActionAccept)
			alice := s.client(aliceToken)
			task, err := send(t, alice, say("c1", "make it"))
			if err != nil {
				t.Fatal(err)
			}
			if task.Status.State != a2a.TaskStateInputRequired || task.Status.Message == nil {
				t.Fatalf("task %s, want input-required", task.Status.State)
			}
			esc, _ := task.Status.Message.Metadata[fronta2a.MetaEscalatedCalls].([]any)
			if len(esc) != 1 {
				t.Fatalf("escalated calls = %v", task.Status.Message.Metadata[fronta2a.MetaEscalatedCalls])
			}
			call, _ := esc[0].(map[string]any)
			if call["name"] != "bash" || call["reason"] == "" {
				t.Errorf("escalated call = %v, want bash with the policy's reason", call)
			}
			if _, err := send(t, s.client(bobToken), a2a.NewMessageForTask(a2a.MessageRoleUser, task, fronta2a.ApprovalPart(fronta2a.Approval{CallID: call["call_id"].(string), Approve: true}))); !errors.Is(err, a2a.ErrTaskNotFound) {
				t.Errorf("bob's approval of alice's call = %v, want not found", err)
			}
			done, err := send(t, alice, a2a.NewMessageForTask(a2a.MessageRoleUser, task, fronta2a.ApprovalPart(fronta2a.Approval{CallID: call["call_id"].(string), Approve: approve})))
			if err != nil {
				t.Fatal(err)
			}
			if done.Status.State != a2a.TaskStateCompleted {
				t.Fatalf("task %s after the answer, want completed", done.Status.State)
			}
			if _, err := os.Stat(filepath.Join(s.rig.dir, "PEER")); (err == nil) != approve {
				t.Errorf("PEER made: %v, want %v", err == nil, approve)
			}
			if asked := v.questions(); len(asked) != 0 {
				t.Errorf("the served session's view was asked %q", asked)
			}
			sess := s.session("c1")
			if got := triggers(sess); !slices.Equal(got, []string{"a2a/alice", "a2a/alice"}) {
				t.Errorf("the runs' triggers = %q, want both the caller's", got)
			}
			// The decision names nobody: the run's trigger names the
			// caller.
			want := "reject by : the caller alice refused this call"
			if approve {
				want = "proceed by : approved by the caller alice"
			}
			if got := decisions(sess); !slices.Contains(got, want) {
				t.Errorf("the conversation's decisions = %q, want %q", got, want)
			}
		})
	}
}

// A caller that cancels a task waiting on its approval leaves nothing
// held: the call is refused in the conversation's session, under the
// caller's principal, the conversation keeps no unanswered call and
// waits on no task, and its next message starts clean.
func TestACanceledEscalationIsSettled(t *testing.T) {
	s := startA2A(t, &steps{parent: [][2]string{{"bash", `{"command":"touch PEER"}`}}}, policy.Rules{}, true)
	alice := s.client(aliceToken)
	task, err := send(t, alice, say("c1", "make it"))
	if err != nil {
		t.Fatal(err)
	}
	if task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("task %s, want input-required", task.Status.State)
	}
	if w, _ := s.peers.store.LoadWaiting(t.Context(), "c1"); w != string(task.ID) {
		t.Fatalf("the conversation waits on %q, want %s", w, task.ID)
	}
	if _, err := alice.CancelTask(t.Context(), &a2a.TaskIDParams{ID: task.ID}); err != nil {
		t.Fatal(err)
	}
	// The settlement runs apart from the cancel's request.
	const refused = "the caller alice canceled the task, so this call was not run"
	deadline := time.Now().Add(15 * time.Second)
	for !slices.ContainsFunc(decisions(s.session("c1")), func(d string) bool { return strings.Contains(d, refused) }) {
		if time.Now().After(deadline) {
			t.Fatalf("the conversation's decisions = %q, want the cancel's refusal", decisions(s.session("c1")))
		}
		time.Sleep(10 * time.Millisecond)
	}
	for {
		w, _ := s.peers.store.LoadWaiting(t.Context(), "c1")
		esc, _ := s.peers.store.LoadEscalated(t.Context(), "c1")
		if w == "" && len(esc) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the cancel the conversation waits on %q with %d escalated calls", w, len(esc))
		}
		time.Sleep(10 * time.Millisecond)
	}
	transcript, err := s.peers.store.Load(t.Context(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	answered := map[string]bool{}
	for _, it := range transcript {
		if o, ok := it.(*openresponses.FunctionCallOutput); ok {
			answered[o.CallID] = true
		}
	}
	for _, it := range transcript {
		if c, ok := it.(*openresponses.FunctionCall); ok && !answered[c.CallID] {
			t.Errorf("the conversation holds the unanswered call %s", c.CallID)
		}
	}
	if _, err := os.Stat(filepath.Join(s.rig.dir, "PEER")); err == nil {
		t.Error("the call ran")
	}
	if got := triggers(s.session("c1")); len(got) < 2 || got[len(got)-1] != "a2a/alice" {
		t.Errorf("the runs' triggers = %q, want the settlement under the caller's", got)
	}
	next, err := send(t, alice, say("c1", "try again"))
	if err != nil {
		t.Fatalf("the conversation's next message: %v", err)
	}
	if st := next.Status.State; st != a2a.TaskStateCompleted && st != a2a.TaskStateInputRequired {
		t.Errorf("the next task %s, want it run", st)
	}
}

// Without -a2a dax serve mounts no A2A; with it, the endpoint is
// behind the token and the card names where serve is reached.
func TestServeMountsA2AOnlyWhenAsked(t *testing.T) {
	get := func(t *testing.T, url, token string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	plain := startServe(t, &steps{}, policy.Rules{})
	if code := get(t, plain.url+a2asrv.WellKnownAgentCardPath, ""); code != http.StatusNotFound {
		t.Errorf("without -a2a, the card = %d, want 404", code)
	}
	if code := get(t, plain.url+a2aPath, plain.token); code != http.StatusNotFound {
		t.Errorf("without -a2a, %s = %d, want 404", a2aPath, code)
	}
	if strings.Contains(plain.out.String(), "a2a:") {
		t.Errorf("without -a2a dax serve said %q", plain.out.String())
	}

	dir := t.TempDir()
	s := startServeFront(t, &steps{}, policy.Rules{}, nil, func(f *serveFront) { f.a2a, f.a2aDir = true, dir })
	if code := get(t, s.url+a2asrv.WellKnownAgentCardPath, ""); code != http.StatusOK {
		t.Errorf("with -a2a, the card = %d, want 200", code)
	}
	if code := get(t, s.url+a2aPath, ""); code != http.StatusUnauthorized {
		t.Errorf("with -a2a, %s without the token = %d, want 401", a2aPath, code)
	}
	if want := "a2a: " + s.url + a2aPath; !strings.Contains(s.out.String(), want) {
		t.Errorf("dax serve's lines lack %q:\n%s", want, s.out.String())
	}
}

// A dax whose config names another dax serve -a2a as a peer calls it,
// through a tunnel to it, as from another machine: the peer's tool
// reaches the served agent, its reply is the call's output, and the
// served side records the conversation in a session of its own.
func TestADaxCallsAServedDax(t *testing.T) {
	s := startServeFront(t, &steps{}, policy.Rules{}, nil, func(f *serveFront) { f.a2a, f.a2aDir = true, t.TempDir() })
	t.Setenv("BUILDER_TOKEN", s.token)
	through := "http://" + tunnel(t, strings.TrimPrefix(s.url, "http://"))
	callerModel := &steps{parent: [][2]string{{"builder", `{"input":"build it"}`}}}
	caller, err := agent.New(t.Context(), agent.Options{
		Model: "scripted", Streamer: callerModel, Dir: t.TempDir(), UserDir: t.TempDir(),
		Store:  agentsession.NewMemoryStore(),
		Policy: &policy.Settings{Builtin: true, Fallback: "ask", User: policy.Rules{Allow: []string{"builder"}}},
		Extensions: []extension.Extension{coding.New(0), daxa2a.New(daxa2a.Options{Remotes: []daxa2a.Remote{
			{Name: "builder", URL: through, TokenEnv: "BUILDER_TOKEN", Relation: daxa2a.Peer},
		}})},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { caller.Close() })
	if !slices.ContainsFunc(caller.Tools(), func(o agentkit.ToolOrigin) bool { return o.Name == "builder" }) {
		t.Fatalf("the served dax is not offered: %v", caller.Tools())
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	end, err := agent.Drive(ctx, caller, agent.Rules{}, openresponses.UserText("go"))
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("the caller's run: %v, %v", end, err)
	}
	var out string
	for _, it := range caller.Agent().State().Transcript {
		if o, ok := it.(*openresponses.FunctionCallOutput); ok {
			out = o.Output.Text
		}
	}
	if !strings.Contains(out, "all done") {
		t.Errorf("the call's output = %q, want the served agent's reply", out)
	}
	listed, err := agent.List(t.Context(), s.rig.o.Root, s.rig.dir)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, sum := range listed {
		if sum.Header.ID == s.rig.sess.ID() {
			continue
		}
		sess, err := s.rig.sess.Recorder().Store().Open(t.Context(), sum.Header.ID)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Equal(userTexts(sess), []string{"build it"}) && slices.Equal(triggers(sess), []string{"a2a/" + servePrincipal}) {
			found = true
		}
	}
	if !found {
		t.Errorf("the served side holds no session of the call's conversation among %d", len(listed))
	}
}
