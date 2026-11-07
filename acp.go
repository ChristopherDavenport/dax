package dax

// This file is the ACP front (-front acp): dax driven by an editor over
// the Agent Client Protocol v1 (#167), JSON-RPC on standard input and
// output (internal/acp). Each ACP session is a dax session under the
// same settings, policy and record: the first session/new takes the
// session the command line opened, each later one is Session.Reopen
// over the live settings, as /new is. session/load resumes one of this
// project's top-level sessions and replays it from the record. The
// editor answers the permissions; its MCP servers start on this machine
// under -trust-mcp alone. Nothing but the protocol goes to standard
// output.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/internal/acp"
	"github.com/ChristopherDavenport/dax/internal/render"
	"github.com/ChristopherDavenport/dax/policy"
	"github.com/ChristopherDavenport/dax/tool"
)

// AtEditor is the person at an editor driving dax over ACP, as who sent
// a prompt or answered a question.
var AtEditor = agentturn.Trigger{Kind: "human", Source: "acp"}

// acpFront serves the editor on in and out (standard input and output
// when nil).
type acpFront struct {
	info frontInfo
	in   io.Reader
	out  io.Writer
	// errw is where notes go: standard error when nil.
	errw io.Writer
	// trustMCP starts the editor's MCP servers (-trust-mcp).
	trustMCP bool
}

func (f *acpFront) stderr() io.Writer {
	if f.errw != nil {
		return f.errw
	}
	return os.Stderr
}

// Prepare sends the session's notes to standard error: standard output
// is the protocol's.
func (f *acpFront) Prepare(o *agent.Options) {
	w := f.stderr()
	o.Log = func(format string, args ...any) { fmt.Fprintf(w, format+"\n", args...) }
}

// acpServer is the front's state: the session the command line opened,
// whether a session/new took it, and the ACP sessions by ID.
type acpServer struct {
	f     *acpFront
	conn  *acp.Conn
	first *agent.Session

	mu       sync.Mutex
	taken    bool
	sessions map[string]*acpSession
	opened   []*agent.Session // the sessions opened here, in order
}

// acpSession is one ACP session: a dax session, a prompt at a time.
type acpSession struct {
	srv  *acpServer
	sess *agent.Session
	// prompt is held by session/prompt; cancel ends the one running.
	prompt sync.Mutex
	cmu    sync.Mutex
	cancel context.CancelFunc
	mode   string
	done   []func()
}

func (f *acpFront) Run(ctx context.Context, sess *agent.Session) error {
	in, out := f.in, f.out
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	srv := &acpServer{f: f, conn: acp.NewConn(out), first: sess, sessions: map[string]*acpSession{}}
	err := srv.conn.Serve(ctx, in, srv.handle)
	srv.close()
	return err
}

// close lets every ACP session go and closes the sessions opened here,
// the first, which the caller closes again (a no-op), included; the
// last opened holds the store, so it is closed last.
func (s *acpServer) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.sessions {
		for _, d := range a.done {
			d()
		}
	}
	if !s.taken && len(s.opened) == 0 {
		return
	}
	all := append([]*agent.Session{s.first}, s.opened...)
	for _, x := range all {
		if err := x.Close(); err != nil {
			fmt.Fprintf(s.f.stderr(), "dax: closing session %s: %v\n", x.ID(), err)
		}
	}
}

func decode[T any](params json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(params, &v); err != nil {
		return v, acp.Errorf(acp.CodeInvalidParams, "%v", err)
	}
	return v, nil
}

func (s *acpServer) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case acp.MethodInitialize:
		return acp.InitializeResponse{
			ProtocolVersion: acp.ProtocolVersion,
			AgentCapabilities: acp.AgentCapabilities{
				LoadSession:        true,
				PromptCapabilities: acp.PromptCapabilities{EmbeddedContext: true},
			},
			AuthMethods: []acp.AuthMethod{},
		}, nil
	case acp.MethodAuthenticate:
		// The model's key is dax's environment's; there is nothing to
		// sign in to.
		return struct{}{}, nil
	case acp.MethodSessionNew:
		req, err := decode[acp.NewSessionRequest](params)
		if err != nil {
			return nil, err
		}
		if err := s.checkCWD(req.CWD); err != nil {
			return nil, err
		}
		sess, err := s.open(ctx, "")
		if err != nil {
			return nil, err
		}
		a := s.add(sess)
		a.startServers(ctx, req.MCPServers)
		return acp.Then{Result: acp.NewSessionResponse{SessionID: sess.ID(), Modes: a.modes()}, After: a.announce}, nil
	case acp.MethodSessionLoad:
		req, err := decode[acp.LoadSessionRequest](params)
		if err != nil {
			return nil, err
		}
		if err := s.checkCWD(req.CWD); err != nil {
			return nil, err
		}
		if err := s.loadable(ctx, req.SessionID); err != nil {
			return nil, err
		}
		sess, err := s.open(ctx, req.SessionID)
		if err != nil {
			return nil, err
		}
		a := s.add(sess)
		if err := a.replay(ctx); err != nil {
			return nil, err
		}
		a.startServers(ctx, req.MCPServers)
		return acp.Then{Result: acp.LoadSessionResponse{Modes: a.modes()}, After: a.announce}, nil
	case acp.MethodSessionPrompt:
		req, err := decode[acp.PromptRequest](params)
		if err != nil {
			return nil, err
		}
		a, err := s.session(req.SessionID)
		if err != nil {
			return nil, err
		}
		return a.run(ctx, req.Prompt)
	case acp.MethodSessionCancel:
		req, err := decode[acp.CancelNotification](params)
		if err != nil {
			return nil, err
		}
		if a, err := s.session(req.SessionID); err == nil {
			a.cancelRun()
		}
		return nil, nil
	case acp.MethodSessionSetMode:
		req, err := decode[acp.SetModeRequest](params)
		if err != nil {
			return nil, err
		}
		a, err := s.session(req.SessionID)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(policy.Cycle, req.ModeID) {
			return nil, acp.Errorf(acp.CodeInvalidParams, "mode %q: want one of %s", req.ModeID, strings.Join(policy.Cycle, ", "))
		}
		if _, err := a.sess.SetMode(ctx, req.ModeID); err != nil {
			return nil, err
		}
		a.modeChanged()
		return struct{}{}, nil
	}
	return nil, acp.Errorf(acp.CodeMethodNotFound, "dax does not take %s", method)
}

// checkCWD takes the directory dax started in, or one inside it, and
// refuses any other: the settings, the project's config, MCP trust and
// the workspace were resolved for that directory.
func (s *acpServer) checkCWD(cwd string) error {
	start := s.f.info.Dir
	if cwd == "" || start == "" {
		return nil
	}
	rel, err := filepath.Rel(start, filepath.Clean(cwd))
	if err != nil || !filepath.IsAbs(cwd) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return acp.Errorf(acp.CodeInvalidParams, "cwd %s: this dax serves %s; start another in that directory", cwd, start)
	}
	return nil
}

// loadable refuses an ID that is not one of this project's top-level
// sessions: never an A2A caller's conversation, never a sub-agent's.
func (s *acpServer) loadable(ctx context.Context, id string) error {
	list, err := s.first.Sessions(ctx)
	if err != nil {
		return err
	}
	for _, sm := range list {
		if sm.Header.ID == id {
			return nil
		}
	}
	return acp.Errorf(acp.CodeInvalidParams, "session %s is not one of this project's", id)
}

// open is the session for a session/new (id "") or a session/load: the
// one the command line opened, for the first new or a load of its own
// ID before any new took it, else one reopened over its live settings.
// A load of a session already open in this process is refused: one
// session, one conversation, so two sessions never record to one.
func (s *acpServer) open(ctx context.Context, id string) (*agent.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.taken && (id == "" || id == s.first.ID()) {
		s.taken = true
		return s.first, nil
	}
	if id != "" && (id == s.first.ID() || slices.ContainsFunc(s.opened, func(o *agent.Session) bool { return o.ID() == id })) {
		return nil, acp.Errorf(acp.CodeInvalidParams, "session %s is already open in this dax", id)
	}
	from := s.first
	if n := len(s.opened); n > 0 {
		from = s.opened[n-1]
	}
	sess, err := from.Reopen(ctx, id)
	if err != nil {
		return nil, err
	}
	s.opened = append(s.opened, sess)
	return sess, nil
}

func (s *acpServer) add(sess *agent.Session) *acpSession {
	a := &acpSession{srv: s, sess: sess}
	a.mode, _ = sess.Mode()
	a.done = append(a.done, sess.Answering(), sess.Subscribe(a.event))
	s.mu.Lock()
	s.sessions[sess.ID()] = a
	s.mu.Unlock()
	return a
}

func (s *acpServer) session(id string) (*acpSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.sessions[id]
	if a == nil {
		return nil, acp.Errorf(acp.CodeInvalidParams, "no session %s", id)
	}
	return a, nil
}

// update sends a session/update.
func (a *acpSession) update(u acp.SessionUpdate) {
	_ = a.srv.conn.Notify(acp.MethodSessionUpdate, acp.SessionNotification{SessionID: a.sess.ID(), Update: u})
}

// startServers starts the editor's MCP servers: stdio ones, under
// -trust-mcp, on this machine.
func (a *acpSession) startServers(ctx context.Context, servers []acp.MCPServer) {
	w := a.srv.f.stderr()
	for _, m := range servers {
		switch {
		case m.Type != "" && m.Type != "stdio":
			fmt.Fprintf(w, "dax: the editor's MCP server %s (%s) is not started: dax takes stdio servers from an editor\n", render.Clean(m.Name), render.Clean(m.Type))
		case !a.srv.f.trustMCP:
			fmt.Fprintf(w, "dax: the editor's MCP server %s is not started: start dax with -trust-mcp to run an editor's servers\n", render.Clean(m.Name))
		default:
			env := map[string]string{}
			for _, e := range m.Env {
				env[e.Name] = e.Value
			}
			if _, err := a.sess.AddEditorMCP(ctx, m.Name, m.Command, m.Args, env); err != nil {
				fmt.Fprintf(w, "dax: the editor's MCP server %s: %v\n", render.Clean(m.Name), render.Clean(err.Error()))
			}
		}
	}
}

// modes are the modes the editor is offered: Shift-Tab's, never
// bypassPermissions.
func (a *acpSession) modes() *acp.SessionModes {
	cur, _ := a.sess.Mode()
	if cur == "" {
		return nil
	}
	out := &acp.SessionModes{CurrentModeID: cur}
	for _, m := range policy.Cycle {
		out.AvailableModes = append(out.AvailableModes, acp.SessionMode{ID: m, Name: m})
	}
	return out
}

// modeChanged tells the editor the mode in force, when it moved.
func (a *acpSession) modeChanged() {
	cur, _ := a.sess.Mode()
	a.cmu.Lock()
	moved := cur != a.mode
	a.mode = cur
	a.cmu.Unlock()
	if moved {
		a.update(acp.SessionUpdate{SessionUpdate: acp.UpdateCurrentMode, CurrentModeID: cur})
	}
}

// announce sends the commands the editor may offer: the prompt
// templates and the MCP servers' prompts. dax's other slash commands
// act on a terminal.
func (a *acpSession) announce() {
	var cmds []acp.AvailableCommand
	for _, p := range a.sess.Templates() {
		cmds = append(cmds, acp.AvailableCommand{Name: p.Name, Description: "prompt template " + p.Path, Input: &acp.CommandInput{Hint: "arguments"}})
	}
	for _, p := range a.sess.MCPPrompts(context.Background()) {
		cmds = append(cmds, acp.AvailableCommand{Name: p.Command, Description: p.Description, Input: &acp.CommandInput{Hint: strings.Join(p.Arguments, " ")}})
	}
	if cmds == nil {
		cmds = []acp.AvailableCommand{}
	}
	a.update(acp.SessionUpdate{SessionUpdate: acp.UpdateAvailableCommands, AvailableCommands: cmds})
}

// replay sends the conversation the record holds, as updates: the
// person's messages, the model's, and each call with how it ended.
func (a *acpSession) replay(ctx context.Context) error {
	items, err := a.sess.Conversation(ctx)
	if err != nil {
		return err
	}
	for _, r := range items {
		switch it := r.Item.(type) {
		case *openresponses.Message:
			text := it.Text()
			if text == "" {
				continue
			}
			c := acp.Text(text)
			switch it.Role {
			case openresponses.RoleUser:
				a.update(acp.SessionUpdate{SessionUpdate: acp.UpdateUserMessage, Content: &c})
			case openresponses.RoleAssistant:
				a.update(acp.SessionUpdate{SessionUpdate: acp.UpdateAgentMessage, Content: &c})
			}
		case *openresponses.FunctionCall:
			a.update(acp.SessionUpdate{SessionUpdate: acp.UpdateToolCall, ToolCallID: it.CallID, Title: it.Name, Kind: toolKind(it.Name), Status: acp.StatusPending, RawInput: rawJSON(it.Arguments)})
		case *openresponses.FunctionCallOutput:
			status := acp.StatusCompleted
			if r.Failed {
				status = acp.StatusFailed
			}
			a.update(acp.SessionUpdate{SessionUpdate: acp.UpdateToolCallUpdate, ToolCallID: it.CallID, Status: status, ToolOutput: textContent(it.Output.String())})
		}
	}
	return nil
}

// event turns the session's events into updates: the model's text and
// reasoning as they stream, each call as it starts and ends, the mode
// when it moves, and a question asked while a call runs (a sub-agent's
// asked call, a memory pass's write) as a permission the editor answers.
func (a *acpSession) event(ctx context.Context, ev agentturn.Event) error {
	switch e := ev.(type) {
	case *agentturn.ItemUpdate:
		switch d := e.Stream.(type) {
		case *openresponses.OutputTextDeltaEvent:
			c := acp.Text(d.Delta)
			a.update(acp.SessionUpdate{SessionUpdate: acp.UpdateAgentMessage, Content: &c})
		case *openresponses.ReasoningSummaryTextDeltaEvent:
			c := acp.Text(d.Delta)
			a.update(acp.SessionUpdate{SessionUpdate: acp.UpdateAgentThought, Content: &c})
		}
	case *agentturn.ToolStart:
		a.update(acp.SessionUpdate{SessionUpdate: acp.UpdateToolCall, ToolCallID: e.CallID, Title: e.Name, Kind: toolKind(e.Name), Status: acp.StatusInProgress, RawInput: agent.Unstamped(e.Args)})
	case *agentturn.ToolEnd:
		if e.Deferred {
			return nil
		}
		status, text := acp.StatusCompleted, tool.Text(e.Result)
		switch {
		case e.Blocked:
			status, text = acp.StatusFailed, e.Reason
		case e.Err != nil:
			status, text = acp.StatusFailed, e.Err.Error()
		}
		a.update(acp.SessionUpdate{SessionUpdate: acp.UpdateToolCallUpdate, ToolCallID: e.CallID, Status: status, ToolOutput: textContent(text)})
	case *agentturn.TurnStart, *agentturn.RunEnd:
		a.modeChanged()
	case *agentturn.Question:
		q := *e
		go a.question(context.WithoutCancel(ctx), q)
	}
	return nil
}

// question asks the editor about a sub-agent's asked call, as a
// permission, and replies with its answer; any other question (a
// tool's form, a remote agent's question for text) is declined, since
// v1 has no way to ask it.
func (a *acpSession) question(ctx context.Context, q agentturn.Question) {
	rctx := agentturn.ContextWithAnswerer(ctx, AtEditor)
	if q.Call == nil {
		_ = a.sess.Reply(rctx, q.ID, agenttool.Answer{Action: agenttool.ActionDecline, Note: "the editor cannot answer this question"})
		return
	}
	offer := a.sess.QuestionOffer(ctx, q)
	tc := acp.SessionUpdate{ToolCallID: q.Call.CallID, Title: render.Clean(q.Elicitation.Message), Kind: toolKind(q.Call.Name), Status: acp.StatusPending, RawInput: agent.Unstamped(q.Call.Args)}
	choice, err := a.ask(ctx, tc, offer.Rule)
	switch {
	case err != nil || choice == acp.RejectOnce:
		_ = a.sess.Reply(rctx, q.ID, agenttool.Answer{Action: agenttool.ActionDecline})
	case choice == acp.AllowAlways:
		if err := a.sess.GrantQuestion(ctx, q, offer.Rule); err != nil {
			fmt.Fprintf(a.srv.f.stderr(), "dax: %v\n", err)
		}
		fallthrough
	default:
		_ = a.sess.Reply(rctx, q.ID, agenttool.Answer{Action: agenttool.ActionAccept, Content: json.RawMessage(`{}`)})
	}
}

// ask puts a call to the editor: allow once, reject, and allow always
// when dax offers a rule for the session (never kept for the project:
// ACP has no confirm step). It returns the kind of the option chosen;
// a cancel is an error.
func (a *acpSession) ask(ctx context.Context, tc acp.SessionUpdate, rule string) (string, error) {
	opts := []acp.PermissionOption{{OptionID: acp.AllowOnce, Name: "Allow", Kind: acp.AllowOnce}}
	if rule != "" {
		opts = append(opts, acp.PermissionOption{OptionID: acp.AllowAlways, Name: "Allow " + rule + " for this session", Kind: acp.AllowAlways})
	}
	opts = append(opts, acp.PermissionOption{OptionID: acp.RejectOnce, Name: "Reject", Kind: acp.RejectOnce})
	var res acp.RequestPermissionResponse
	if err := a.srv.conn.Request(ctx, acp.MethodRequestPermission, acp.RequestPermissionRequest{SessionID: a.sess.ID(), ToolCall: tc, Options: opts}, &res); err != nil {
		return "", err
	}
	if res.Outcome.Outcome != "selected" {
		return "", errors.New("cancelled")
	}
	for _, o := range opts {
		if o.OptionID == res.Outcome.OptionID {
			return o.Kind, nil
		}
	}
	return "", fmt.Errorf("the editor chose %q, which dax did not offer", res.Outcome.OptionID)
}

// cancelRun aborts the run of the prompt in flight, and the
// permissions it waits on.
func (a *acpSession) cancelRun() {
	a.cmu.Lock()
	cancel := a.cancel
	a.cmu.Unlock()
	if cancel != nil {
		cancel()
	}
	a.sess.Abort()
}

// run is one session/prompt: the prompt, then each permission the run
// ends on put to the editor and the run resumed with the answers, until
// it ends.
func (a *acpSession) run(ctx context.Context, blocks []acp.ContentBlock) (acp.PromptResponse, error) {
	a.prompt.Lock()
	defer a.prompt.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.cmu.Lock()
	a.cancel = cancel
	a.cmu.Unlock()
	defer func() {
		a.cmu.Lock()
		a.cancel = nil
		a.cmu.Unlock()
	}()
	items, reply, err := a.items(ctx, blocks)
	if err != nil {
		return acp.PromptResponse{}, err
	}
	if reply != "" {
		c := acp.Text(reply)
		a.update(acp.SessionUpdate{SessionUpdate: acp.UpdateAgentMessage, Content: &c})
		return acp.PromptResponse{StopReason: acp.StopEndTurn}, nil
	}
	byPerson := agentturn.ContextWithTrigger(ctx, AtEditor)
	end, err := a.sess.Prompt(byPerson, items...)
	if errors.Is(err, agent.ErrHeld) {
		// Calls held when the session stopped are answered first, and
		// the prompt steers the run their answers resume.
		answers, aerr := a.answer(ctx, agent.HeldPermissions(a.sess))
		if aerr != nil {
			return a.stopped(ctx)
		}
		if err = a.sess.Queue(byPerson, agentturn.QueueSteer, items...); err == nil {
			end, err = a.sess.Resume(byPerson, answers...)
		}
	}
	for err == nil && end.Reason == agentturn.ReasonInputRequired {
		perms := agent.PermissionsOf(a.sess, end)
		if len(perms) == 0 {
			break
		}
		answers, aerr := a.answer(ctx, perms)
		if aerr != nil {
			return a.stopped(ctx)
		}
		end, err = a.sess.Resume(byPerson, answers...)
	}
	if ctx.Err() != nil {
		return acp.PromptResponse{StopReason: acp.StopCancelled}, nil
	}
	if err != nil {
		if s, ok := render.Explain(err, false); ok {
			return acp.PromptResponse{}, errors.New(s)
		}
		return acp.PromptResponse{}, err
	}
	switch {
	case end.Reason == agentturn.ReasonAborted:
		return acp.PromptResponse{StopReason: acp.StopCancelled}, nil
	case end.Reason == agentturn.ReasonStopped && end.Cause == agentturn.StopMaxTurns:
		return acp.PromptResponse{StopReason: acp.StopMaxTurnRequests}, nil
	case end.Reason == agentturn.ReasonStopped && end.Cause == agentturn.StopGuard:
		return acp.PromptResponse{StopReason: acp.StopRefusal}, nil
	}
	return acp.PromptResponse{StopReason: acp.StopEndTurn}, nil
}

// stopped is a prompt the editor cancelled while a permission waited:
// the run's calls stay pending, refused at the next prompt as cut off.
func (a *acpSession) stopped(context.Context) (acp.PromptResponse, error) {
	return acp.PromptResponse{StopReason: acp.StopCancelled}, nil
}

// answer puts each permission to the editor, in order: an approval, a
// session grant first for "allow always", or a refusal that lets the
// model go on and tells it why (#150).
func (a *acpSession) answer(ctx context.Context, perms []agent.Permission) ([]agentturn.Answer, error) {
	var out []agentturn.Answer
	for _, p := range perms {
		offer := a.sess.GrantOffer(ctx, p.Call.CallID)
		tc := acp.SessionUpdate{ToolCallID: p.Call.CallID, Title: render.Clean(p.Call.Name + ": " + p.Reason), Kind: toolKind(p.Call.Name), Status: acp.StatusPending, RawInput: rawJSON(p.Call.Arguments)}
		choice, err := a.ask(ctx, tc, offer.Rule)
		if err != nil {
			return nil, err
		}
		switch choice {
		case acp.AllowAlways:
			if err := a.sess.Grant(ctx, p.Call.CallID, agent.GrantSession, offer.Rule); err != nil {
				fmt.Fprintf(a.srv.f.stderr(), "dax: %v\n", err)
			}
			fallthrough
		case acp.AllowOnce:
			out = append(out, agentturn.Approve(p.Call.CallID).WithBy(agentpolicy.ByHuman))
		default:
			out = append(out, agent.Refusal(p.Call.CallID, "", false))
		}
	}
	return out, nil
}

// items is a prompt's content as the session takes it: text, the text
// of an embedded resource marked with its URI, a link as its URI. A
// prompt that is a slash command runs a prompt template's or an MCP
// prompt's; reply is what dax says instead of running any other.
func (a *acpSession) items(ctx context.Context, blocks []acp.ContentBlock) (items []openresponses.Item, reply string, err error) {
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "resource_link":
			parts = append(parts, b.URI)
		case "resource":
			if b.Resource != nil && b.Resource.Text != "" {
				parts = append(parts, "<resource uri=\""+b.Resource.URI+"\">\n"+b.Resource.Text+"\n</resource>")
			}
		default:
			return nil, "", acp.Errorf(acp.CodeInvalidParams, "a %s block: dax takes text and resources from an editor", b.Type)
		}
	}
	text := strings.Join(parts, "\n")
	if strings.HasPrefix(text, "/") && !strings.HasPrefix(text, "//") {
		name, _, _ := strings.Cut(text, " ")
		name = strings.TrimPrefix(name, "/")
		known := slices.ContainsFunc(a.sess.Templates(), func(p agent.PromptTemplate) bool { return p.Name == name }) ||
			slices.ContainsFunc(a.sess.MCPPrompts(ctx), func(p agent.MCPPrompt) bool { return p.Command == name })
		if !known {
			return nil, "dax's slash commands act on its terminal; over ACP it runs prompt templates and MCP prompts alone.", nil
		}
		r, err := command(ctx, a.sess, a.sess, text, false)
		if err != nil {
			return nil, "", err
		}
		return r.send, "", nil
	}
	return []openresponses.Item{openresponses.UserText(userText(text))}, "", nil
}

// toolKind is the kind of call an editor shows for a tool.
func toolKind(name string) string {
	switch name {
	case "read", "ls", "read_output", "skill":
		return "read"
	case "glob", "grep", "memory_search", "conversation_search":
		return "search"
	case "write", "edit":
		return "edit"
	case "bash", "background":
		return "execute"
	case "explore", "task":
		return "think"
	case "exit_plan_mode":
		return "switch_mode"
	}
	return "other"
}

// rawJSON is s as raw JSON, or as a JSON string when it is not JSON.
func rawJSON(s string) json.RawMessage {
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	b, _ := json.Marshal(s)
	return b
}

// textContent is text as a tool call's content.
func textContent(text string) []acp.ToolCallContent {
	if len(text) > maxACPOutput {
		text = text[:maxACPOutput] + "…"
	}
	return []acp.ToolCallContent{{Type: "content", Content: acp.Text(text)}}
}

// maxACPOutput is the most of a call's output an update carries.
const maxACPOutput = 64 << 10
