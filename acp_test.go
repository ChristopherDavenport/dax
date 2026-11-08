package dax

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/ext/coding"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/internal/acp"
	"github.com/ChristopherDavenport/dax/policy"
)

// editor is a scripted ACP client in this process: it sends requests on
// dax's input, reads dax's output, keeps the session updates, and
// answers each permission with permit.
type editor struct {
	t      *testing.T
	in     *io.PipeWriter
	mu     sync.Mutex
	next   int
	wait   map[string]chan rpcMsg
	update []acp.SessionUpdate
	asked  []acp.RequestPermissionRequest
	permit func(acp.RequestPermissionRequest) acp.RequestPermissionResponse
	done   chan error
}

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *acp.Error      `json:"error,omitempty"`
}

// startACP runs dax's ACP front over sess, its notes discarded.
func startACP(t *testing.T, sess *agent.Session, dir string) *editor {
	t.Helper()
	return startACPFront(t, sess, &acpFront{info: frontInfo{Dir: dir}, errw: io.Discard})
}

// startACPFront runs f over sess, on pipes of the editor's.
func startACPFront(t *testing.T, sess *agent.Session, f *acpFront) *editor {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	e := &editor{t: t, in: inW, wait: map[string]chan rpcMsg{}, done: make(chan error, 1)}
	f.in, f.out = inR, outW
	go func() {
		err := f.Run(context.Background(), sess)
		outW.Close()
		e.done <- err
	}()
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			var m rpcMsg
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				t.Errorf("dax wrote %q to standard output: %v", sc.Text(), err)
				continue
			}
			switch {
			case m.Method == acp.MethodSessionUpdate:
				var n acp.SessionNotification
				json.Unmarshal(m.Params, &n)
				e.mu.Lock()
				e.update = append(e.update, n.Update)
				e.mu.Unlock()
			case m.Method == acp.MethodRequestPermission:
				var req acp.RequestPermissionRequest
				json.Unmarshal(m.Params, &req)
				e.mu.Lock()
				e.asked = append(e.asked, req)
				permit := e.permit
				e.mu.Unlock()
				res, _ := json.Marshal(permit(req))
				e.send(rpcMsg{JSONRPC: "2.0", ID: m.ID, Result: res})
			case m.Method == "":
				e.mu.Lock()
				ch := e.wait[string(m.ID)]
				e.mu.Unlock()
				if ch != nil {
					ch <- m
				}
			}
		}
	}()
	return e
}

func (e *editor) send(m rpcMsg) {
	b, _ := json.Marshal(m)
	if _, err := e.in.Write(append(b, '\n')); err != nil {
		e.t.Errorf("writing to dax: %v", err)
	}
}

// call sends a request and waits for its answer.
func (e *editor) call(method string, params any) rpcMsg {
	e.t.Helper()
	b, _ := json.Marshal(params)
	e.mu.Lock()
	e.next++
	id := strconv.Itoa(e.next)
	ch := make(chan rpcMsg, 1)
	e.wait[id] = ch
	e.mu.Unlock()
	e.send(rpcMsg{JSONRPC: "2.0", ID: json.RawMessage(id), Method: method, Params: b})
	select {
	case m := <-ch:
		return m
	case <-time.After(30 * time.Second):
		e.t.Fatalf("%s: no answer", method)
	}
	return rpcMsg{}
}

func (e *editor) notify(method string, params any) {
	b, _ := json.Marshal(params)
	e.send(rpcMsg{JSONRPC: "2.0", Method: method, Params: b})
}

// updates are the session updates so far, and clears them.
func (e *editor) updates() []acp.SessionUpdate {
	e.mu.Lock()
	defer e.mu.Unlock()
	u := e.update
	e.update = nil
	return u
}

func (e *editor) setPermit(fn func(acp.RequestPermissionRequest) acp.RequestPermissionResponse) {
	e.mu.Lock()
	e.permit = fn
	e.mu.Unlock()
}

func (e *editor) askedCalls() []acp.RequestPermissionRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]acp.RequestPermissionRequest(nil), e.asked...)
}

func (e *editor) stop() {
	e.in.Close()
	select {
	case <-e.done:
	case <-time.After(30 * time.Second):
		e.t.Fatal("the ACP front did not end with its input")
	}
}

func has(us []acp.SessionUpdate, kind, text string) bool {
	for _, u := range us {
		if u.SessionUpdate != kind {
			continue
		}
		b, _ := json.Marshal(u)
		if strings.Contains(string(b), text) {
			return true
		}
	}
	return false
}

// An editor drives dax over ACP: a prompt whose call asks is answered by
// the editor's permission, a cancel ends the next prompt's run while its
// permission waits, a cwd outside the start directory is refused, and the
// session loads in a new process, replayed from the record. (#167)
func TestAnEditorDrivesDaxOverACP(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	o := agent.Options{
		Model: "scripted", Streamer: &steps{parent: [][2]string{{"write", `{"path":"new.txt","content":"x"}`}}},
		Dir: dir, Root: filepath.Join(base, "sessions"), UserDir: filepath.Join(base, "user"),
		Extensions: []extension.Extension{coding.New(0)},
		Policy:     &policy.Settings{Builtin: true, Fallback: "ask"},
	}
	ctx := context.Background()
	sess, err := agent.New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	e := startACP(t, sess, dir)
	e.setPermit(func(req acp.RequestPermissionRequest) acp.RequestPermissionResponse {
		return acp.RequestPermissionResponse{Outcome: acp.PermissionOutcome{Outcome: "selected", OptionID: acp.AllowOnce}}
	})
	var init acp.InitializeResponse
	json.Unmarshal(e.call(acp.MethodInitialize, acp.InitializeRequest{ProtocolVersion: 1}).Result, &init)
	if init.ProtocolVersion != 1 || !init.AgentCapabilities.LoadSession {
		t.Fatalf("initialize: %+v", init)
	}
	if m := e.call(acp.MethodSessionNew, acp.NewSessionRequest{CWD: filepath.Join(base, "elsewhere")}); m.Error == nil || !strings.Contains(m.Error.Message, "serves "+dir) {
		t.Errorf("a cwd outside the start: %+v", m.Error)
	}
	if m := e.call(acp.MethodSessionNew, acp.NewSessionRequest{}); m.Error == nil || m.Error.Code != acp.CodeInvalidParams {
		t.Errorf("no cwd: %+v, want refused", m.Error)
	}
	var created acp.NewSessionResponse
	json.Unmarshal(e.call(acp.MethodSessionNew, acp.NewSessionRequest{CWD: dir}).Result, &created)
	if created.SessionID != sess.ID() || created.Modes == nil || created.Modes.CurrentModeID != policy.ModeDefault {
		t.Fatalf("session/new: %+v, want the opened session %s in default mode", created, sess.ID())
	}
	for _, m := range created.Modes.AvailableModes {
		if m.ID == policy.ModeBypass {
			t.Error("bypassPermissions is offered to the editor")
		}
	}
	var res acp.PromptResponse
	json.Unmarshal(e.call(acp.MethodSessionPrompt, acp.PromptRequest{SessionID: created.SessionID, Prompt: []acp.ContentBlock{acp.Text("make a file")}}).Result, &res)
	if res.StopReason != acp.StopEndTurn {
		t.Errorf("stop reason %q", res.StopReason)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); err != nil {
		t.Errorf("the approved write did not run: %v", err)
	}
	us := e.updates()
	asked := e.askedCalls()
	if len(asked) != 1 || asked[0].ToolCall.ToolCallID == "" || !has(us, acp.UpdateToolCall, `"kind":"edit"`) || !has(us, acp.UpdateAgentMessage, "all done") || !has(us, acp.UpdateAvailableCommands, "") || has(us, acp.UpdateToolCall, "dax_stamp") {
		t.Errorf("asked %d, updates %+v", len(asked), us)
	}

	// A cancel while the permission waits: the editor cancels, then
	// answers the request cancelled, as ACP asks.
	e.setPermit(func(req acp.RequestPermissionRequest) acp.RequestPermissionResponse {
		e.notify(acp.MethodSessionCancel, acp.CancelNotification{SessionID: req.SessionID})
		return acp.RequestPermissionResponse{Outcome: acp.PermissionOutcome{Outcome: "cancelled"}}
	})
	json.Unmarshal(e.call(acp.MethodSessionPrompt, acp.PromptRequest{SessionID: created.SessionID, Prompt: []acp.ContentBlock{acp.Text("again")}}).Result, &res)
	if res.StopReason != acp.StopCancelled {
		t.Errorf("after a cancel: stop reason %q", res.StopReason)
	}
	e.stop()

	// A new process: a session over the same store, which loads the
	// first and replays it.
	o.Streamer = &steps{}
	sess2, err := agent.New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer sess2.Close()
	e2 := startACP(t, sess2, dir)
	e2.call(acp.MethodInitialize, acp.InitializeRequest{ProtocolVersion: 1})
	if m := e2.call(acp.MethodSessionLoad, acp.LoadSessionRequest{SessionID: "not-a-session", CWD: dir}); m.Error == nil {
		t.Error("an unknown session loaded")
	}
	if m := e2.call(acp.MethodSessionLoad, acp.LoadSessionRequest{SessionID: created.SessionID, CWD: dir}); m.Error != nil {
		t.Fatalf("session/load: %+v", m.Error)
	}
	us = e2.updates()
	for _, want := range []struct{ kind, text string }{
		{acp.UpdateUserMessage, "make a file"},
		{acp.UpdateToolCall, `"title":"write"`},
		{acp.UpdateToolCallUpdate, `"status":"completed"`},
		{acp.UpdateAgentMessage, "all done"},
	} {
		if !has(us, want.kind, want.text) {
			t.Errorf("the replay has no %s with %s: %+v", want.kind, want.text, us)
		}
	}
	// One session, one conversation: the loaded session is live in this
	// process, so loading it again, after other sessions were opened, is
	// refused rather than opening a second session that records to it.
	for range 2 {
		if m := e2.call(acp.MethodSessionNew, acp.NewSessionRequest{CWD: dir}); m.Error != nil {
			t.Fatalf("session/new: %+v", m.Error)
		}
	}
	if m := e2.call(acp.MethodSessionLoad, acp.LoadSessionRequest{SessionID: created.SessionID, CWD: dir}); m.Error == nil || m.Error.Code != acp.CodeInvalidParams || !strings.Contains(m.Error.Message, "already open") {
		t.Errorf("a second load of an open session: %+v, want refused as already open", m.Error)
	}
	e2.stop()
}
