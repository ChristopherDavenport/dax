package agent

import (
	"context"
	"fmt"
	"github.com/ChristopherDavenport/dax/workspace"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentmemory/filestore"
	"github.com/ChristopherDavenport/agentsession/cas"
	"github.com/ChristopherDavenport/agentsmd"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/compact"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/ChristopherDavenport/openresponses/echo"

	"github.com/ChristopherDavenport/dax/ext/agents"
	"github.com/ChristopherDavenport/dax/ext/coding"
	"github.com/ChristopherDavenport/dax/ext/memory"
	"github.com/ChristopherDavenport/dax/ext/skills"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/policy"
)

// forced is the echo model made to call one tool by name, then answer
// once the call has an output.
type forced struct {
	echo.Adapter
	name string
}

func (f *forced) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	req.ToolChoice = openresponses.ToolChoice{Function: &openresponses.FunctionToolChoice{Name: f.name}}
	return f.Adapter.CreateStream(ctx, req, sink)
}

// scripted makes one call per step of a run, the step being the number
// of tool outputs since the last user message, then answers "done".
type scripted struct{ calls [][2]string }

func (m *scripted) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	step := 0
	for _, it := range req.Input {
		switch v := it.(type) {
		case *openresponses.Message:
			if v.Role == openresponses.RoleUser {
				step = 0
			}
		case *openresponses.FunctionCallOutput:
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

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// options is a session in a fresh project, user directory and store,
// on the given model, with every layer on but the sub-agents:
// dax-coding, dax-skills and dax-memory.
func options(t *testing.T, model openresponses.Streamer) Options {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "project")
	o := Options{
		Model:    "echo",
		Streamer: model,
		Dir:      dir,
		Root:     filepath.Join(base, "sessions"),
		UserDir:  filepath.Join(base, "user"),
		AgentsMD: true,
		Extensions: []extension.Extension{
			coding.New(0),
			skills.New(skills.Options{}),
			memory.New(filepath.Join(base, "user", "memory")),
		},
	}
	write(t, filepath.Join(o.Dir, "AGENTS.md"), "Run go test before saying done.\n")
	write(t, filepath.Join(o.UserDir, "skills", "greet", "SKILL.md"),
		"---\nname: greet\ndescription: How to greet the user.\nallowed-tools: Bash(echo:*)\n---\nSay hello.\n")
	return o
}

// confirmPolicy is dax's shipped policy: reads run, writes and
// commands ask.
func confirmPolicy(t *testing.T) *policy.Settings {
	t.Helper()
	return &policy.Settings{Builtin: true, Fallback: "ask"}
}

// withAgents adds dax-agents to o, as -agents does, on the sub-agent
// model sub (empty: the main model).
func withAgents(o Options, sub string) Options {
	o.Extensions = append(slices.Clone(o.Extensions), agents.New(agents.Options{Model: sub}))
	return o
}

// trustSkills replaces o's dax-skills with one that trusts the user's
// skills, as -trust-skills does.
func trustSkills(o Options) Options {
	o.Extensions = slices.Clone(o.Extensions)
	for i, e := range o.Extensions {
		if e.Name == skills.Name {
			o.Extensions[i] = skills.New(skills.Options{Trust: true})
		}
	}
	return o
}

// projected closes the session, which releases it in the store, and
// returns its JSONL projection.
func projected(t *testing.T, o Options, s *Session) []byte {
	t.Helper()
	id := s.ID()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	path, err := Project(context.Background(), o.Root, id, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestTheKitAssemblesEveryLayer(t *testing.T) {
	ctx := context.Background()
	o := options(t, &echo.Adapter{})
	mem, err := filestore.Open(filepath.Join(o.UserDir, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Put(ctx, agentmemory.Entry{Scope: "user", Name: "style", Content: "Short answers."}); err != nil {
		t.Fatal(err)
	}

	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// The memory block is a group of parts, one per entry, so a write
	// is recorded as the entry that moved. The layers are counted by
	// the group each part belongs to.
	var ids, layers []string
	for _, p := range s.Kit.Parts() {
		ids = append(ids, p.ID)
		layer := p.ID
		if p.ID == agentkit.PartMemory || strings.HasPrefix(p.ID, "memory/") || strings.HasPrefix(p.ID, "memory:") {
			layer = agentkit.PartMemory
		}
		if len(layers) == 0 || layers[len(layers)-1] != layer {
			layers = append(layers, layer)
		}
	}
	want := []string{agentkit.PartProduct, agentkit.PartSkills, agentkit.PartMemory, agentsmd.PartID}
	if !slices.Equal(layers, want) {
		t.Errorf("layers = %v, want %v", layers, want)
	}
	for _, id := range []string{agentmemory.PartID("user", "style"), agentkit.PartMemoryUsage} {
		if !slices.Contains(ids, id) {
			t.Errorf("parts %v lack %s", ids, id)
		}
	}
	instr := s.Agent.Config().Instructions
	for _, sub := range []string{"You are dax", "greet", "Short answers.", "Run go test before saying done."} {
		if !strings.Contains(instr, sub) {
			t.Errorf("instructions lack %q", sub)
		}
	}
	if strings.Contains(instr, "let sub-agents do") {
		t.Error("instructions guide to sub-agents the session does not offer")
	}
	var names []string
	for _, tl := range s.Tools() {
		names = append(names, tl.Name)
	}
	for _, n := range []string{"read", "write", "edit", "bash", "skill", "memory_save"} {
		if !slices.Contains(names, n) {
			t.Errorf("tools %v lack %s", names, n)
		}
	}

	end, err := s.Prompt(ctx, "go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if end.Reason != agentturn.ReasonDone {
		t.Fatalf("run ended %s", end.Reason)
	}
	id := s.ID()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	n, failed, err := Verify(ctx, o.Root, id)
	if err != nil || len(failed) > 0 || n == 0 {
		t.Fatalf("verify: %d responses, failed %v, err %v", n, failed, err)
	}
}

func TestConfirmAsksAndRecordsTheHuman(t *testing.T) {
	for _, approve := range []bool{true, false} {
		t.Run(map[bool]string{true: "approve", false: "deny"}[approve], func(t *testing.T) {
			ctx := context.Background()
			o := options(t, &forced{name: "bash"})
			o.Policy = confirmPolicy(t)
			var asked []string
			o.Approve = func(c *openresponses.FunctionCall, reason string) bool {
				asked = append(asked, c.Name+": "+reason)
				return approve
			}
			s, err := New(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()

			end, err := s.Prompt(ctx, "echo confirmed")
			if err != nil {
				t.Fatal(err)
			}
			if end.Reason != agentturn.ReasonDone {
				t.Fatalf("run ended %s", end.Reason)
			}
			if len(asked) != 1 || !strings.HasPrefix(asked[0], "bash: ") {
				t.Fatalf("asked %v, want one question about bash", asked)
			}
			var out string
			for _, it := range s.Agent.State().Transcript {
				if o, ok := it.(*openresponses.FunctionCallOutput); ok {
					out = o.Output.Text
				}
			}
			if approve != strings.Contains(out, "confirmed") || approve == (out == deniedOutput) {
				t.Errorf("approve=%v, bash output %q", approve, out)
			}
			data := projected(t, o, s)
			if !strings.Contains(string(data), `"by":"human"`) {
				t.Errorf("session records no human decision")
			}
			if !strings.Contains(string(data), "agentpolicy") {
				t.Errorf("session records no policy verdict")
			}
		})
	}
}

func TestATrustedSkillGrantsItsToolsUntilTheNextMessage(t *testing.T) {
	ctx := context.Background()
	model := &scripted{calls: [][2]string{
		{"skill", `{"name":"greet"}`},
		{"bash", `{"command":"echo hello"}`},
	}}
	o := options(t, model)
	o = trustSkills(o)
	o.Policy = confirmPolicy(t)
	var asked []string
	o.Approve = func(c *openresponses.FunctionCall, _ string) bool {
		asked = append(asked, c.Arguments)
		return true
	}
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.Prompt(ctx, "greet me"); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 0 {
		t.Fatalf("asked about %v after the skill granted Bash(echo:*)", asked)
	}
	// The next message ends the grant, so bash without the skill read
	// first falls back to the policy's default and asks. (Under an
	// explicit ask rule on bash, agentkit v0.0.7 refuses with a reason
	// naming the skill instead; dax leaves bash to the default so that
	// a user's allow rule can override it.)
	model.calls = model.calls[1:]
	if _, err := s.Prompt(ctx, "again"); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0] != `{"command":"echo hello"}` {
		t.Fatalf("asked %v on the second message, want one question", asked)
	}
	// Reading the skill again grants again.
	model.calls = [][2]string{{"skill", `{"name":"greet"}`}, {"bash", `{"command":"echo again"}`}}
	if _, err := s.Prompt(ctx, "greet me again"); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 {
		t.Fatalf("asked %d time(s) in all, want the one from the second message", len(asked))
	}
}

func TestAFoldIsNotedWhileTheSessionRecordsIt(t *testing.T) {
	ctx := context.Background()
	o := options(t, &echo.Adapter{})
	o.Compact = 1
	var notes []string
	o.Log = func(format string, args ...any) { notes = append(notes, fmt.Sprintf(format, args...)) }
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, p := range []string{"one", "two", "three", "four"} {
		if _, err := s.Prompt(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	folds, skipped := 0, 0
	for _, n := range notes {
		switch {
		case strings.HasPrefix(n, "[compacted:"):
			folds++
		case strings.HasPrefix(n, "[not compacted:"):
			skipped++
		}
	}
	data := projected(t, o, s)
	recorded := strings.Count(string(data), `"type":"compaction"`)
	failed := strings.Count(string(data), `"agentturn:compaction_failed"`)
	if folds+skipped == 0 || folds != recorded || skipped != failed {
		t.Errorf("%d fold(s) and %d given up noted, %d and %d recorded; want the same, at least one", folds, skipped, recorded, failed)
	}
}

// bloated is the echo model, but it answers every summary request with
// more text than it was asked to fold.
type bloated struct {
	echo.Adapter
	summaries int
}

func (m *bloated) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	n := len(req.Input)
	if msg, ok := req.Input[n-1].(*openresponses.Message); !ok || len(req.Tools) > 0 || msg.Text() != compact.DefaultSummaryPrompt {
		return m.Adapter.CreateStream(ctx, req, sink)
	}
	m.summaries++
	em := openresponses.NewEmitter(sink, openresponses.NewResponse(req))
	out, err := em.Message(openresponses.PhaseFinalAnswer)
	if err != nil {
		return err
	}
	if err := out.Text(strings.Repeat("The user said something and the agent answered it. ", 4*compact.Estimate(req.Input)+10)); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return em.Complete()
}

func TestASummaryLargerThanItsPrefixIsGivenUpNotFailed(t *testing.T) {
	ctx := context.Background()
	model := &bloated{}
	o := options(t, model)
	o.Compact = 1
	var notes []string
	o.Log = func(format string, args ...any) { notes = append(notes, fmt.Sprintf(format, args...)) }
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, p := range []string{"one", "two"} {
		end, err := s.Prompt(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if end.Reason != agentturn.ReasonDone {
			t.Fatalf("run ended %s, want done with the transcript sent unfolded", end.Reason)
		}
	}
	var given []string
	for _, n := range notes {
		if strings.HasPrefix(n, "[not compacted:") {
			given = append(given, n)
		}
	}
	if len(given) == 0 || !strings.Contains(given[0], compact.ErrSummaryTooLarge.Error()) {
		t.Fatalf("notes %q, want a fold given up as too large", notes)
	}
	data := string(projected(t, o, s))
	if strings.Contains(data, `"type":"compaction"`) {
		t.Errorf("an oversized summary was applied")
	}
	if !strings.Contains(data, `"agentturn:compaction_failed"`) || !strings.Contains(data, `"attempts":2`) || !strings.Contains(data, `"output_types":["message"]`) {
		t.Errorf("the failed fold's record does not say what its two calls answered")
	}
}

func TestACallCutOffInFlightIsResumedAsAborted(t *testing.T) {
	ctx := context.Background()
	model := &scripted{calls: [][2]string{{"bash", `{"command":"sleep 30"}`}}}
	o := options(t, model)
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	s.Agent.Subscribe(func(_ context.Context, ev agentturn.Event) error {
		if _, ok := ev.(*agentturn.ToolDispatch); ok {
			go func() { time.Sleep(200 * time.Millisecond); s.Agent.Abort() }()
		}
		return nil
	})
	end, err := s.Prompt(ctx, "sleep")
	if err != nil {
		t.Fatal(err)
	}
	if end.Reason != agentturn.ReasonAborted {
		t.Fatalf("run ended %s, want aborted", end.Reason)
	}
	id := s.ID()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	model.calls = nil
	r, err := Resume(ctx, o, id)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	pending := r.Pending()
	if len(pending) != 1 || pending[0].Reason != agentturn.PendingAborted || pending[0].IdempotencyKey == "" {
		t.Fatalf("resumed pending = %+v, want one aborted bash call with its key", pending)
	}
	if _, err := r.Prompt(ctx, "what happened?"); err != nil {
		t.Fatal(err)
	}
	var out string
	for _, it := range r.Agent.State().Transcript {
		if o, ok := it.(*openresponses.FunctionCallOutput); ok && o.CallID == pending[0].Call.CallID {
			out = o.Output.Text
		}
	}
	if out != abortedOutput {
		t.Errorf("the model was told %q, want %q", out, abortedOutput)
	}
	data := projected(t, o, r)
	if !strings.Contains(string(data), `"verdict":"answer"`) {
		t.Errorf("the output for a call that may have run is not recorded as an answer")
	}
	n, failed, err := Verify(ctx, o.Root, id)
	if err != nil || len(failed) > 0 || n == 0 {
		t.Fatalf("verify: %d responses, failed %v, err %v", n, failed, err)
	}
}

// summaries is the echo model, noting the reasoning setting of each
// summary request compaction sends.
type summaries struct {
	echo.Adapter
	reasoning []openresponses.ReasoningConfig
}

func (m *summaries) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	if n := len(req.Input); n > 0 && len(req.Tools) == 0 {
		if msg, ok := req.Input[n-1].(*openresponses.Message); ok && msg.Text() == compact.DefaultSummaryPrompt {
			m.reasoning = append(m.reasoning, req.Reasoning)
		}
	}
	return m.Adapter.CreateStream(ctx, req, sink)
}

func TestASummaryIsAskedWithoutReasoning(t *testing.T) {
	ctx := context.Background()
	for _, think := range []bool{false, true} {
		model := &summaries{}
		o := options(t, model)
		o.Compact, o.Think = 1, think
		s, err := New(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"one", "two", "three", "four"} {
			if _, err := s.Prompt(ctx, p); err != nil {
				t.Fatal(err)
			}
		}
		s.Close()
		if len(model.reasoning) == 0 {
			t.Fatal("no summary was asked for")
		}
		for _, r := range model.reasoning {
			if r.Effort != openresponses.ReasoningEffortNone {
				t.Errorf("-think=%v: a summary was asked with reasoning %+v, want effort none", think, r)
			}
		}
	}
}

// thinker reasons before each answer and notes the reasoning each
// request carried, by the encrypted content naming its model.
type thinker struct{ sent [][]string }

func (m *thinker) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	var carried []string
	for _, it := range req.Input {
		if r, ok := it.(*openresponses.ReasoningItem); ok {
			carried = append(carried, r.EncryptedContent)
		}
	}
	m.sent = append(m.sent, carried)
	em := openresponses.NewEmitter(sink, openresponses.NewResponse(req))
	rw, err := em.Reasoning()
	if err != nil {
		return err
	}
	rw.EncryptedContent(req.Model)
	if err := rw.Close(); err != nil {
		return err
	}
	msg, err := em.Message(openresponses.PhaseFinalAnswer)
	if err != nil {
		return err
	}
	if err := msg.Text("ok"); err != nil {
		return err
	}
	if err := msg.Close(); err != nil {
		return err
	}
	return em.Complete()
}

func TestAModelSwitchLeavesTheOldReasoningOutAndTheSecondResponseStillVerifies(t *testing.T) {
	ctx := context.Background()
	model := &thinker{}
	o := options(t, model)
	o.Model, o.Think = "a", true
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prompt(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetModel("b"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prompt(ctx, "two"); err != nil {
		t.Fatal(err)
	}
	id := s.ID()
	s.Close()
	if got := model.sent[1]; len(got) != 0 {
		t.Errorf("model b was sent reasoning %q", got)
	}
	// agentsession v0.0.20 (#56): the recorder writes an omit rule for
	// the reasoning another model wrote, so the second response is
	// hashed over the request as sent and verifies.
	n, failed, err := Verify(ctx, o.Root, id)
	if err != nil || n != 2 || len(failed) != 0 {
		t.Fatalf("verify: %d response(s), failed %v, err %v; want both hashed", n, failed, err)
	}
	causes, err := Unhashed(ctx, o.Root, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(causes) != 0 {
		t.Fatalf("causes %+v, want none: the omitted reasoning no longer leaves the request unhashed", causes)
	}
}

// names records the model name each request carried, the explore
// child's apart from the parent's.
type names struct {
	scripted
	mu            sync.Mutex
	parent, child []string
}

func (m *names) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	m.mu.Lock()
	if strings.Contains(req.Instructions, "read-only explorer") {
		m.child = append(m.child, req.Model)
	} else {
		m.parent = append(m.parent, req.Model)
	}
	m.mu.Unlock()
	return m.scripted.CreateStream(ctx, req, sink)
}

func TestTheSubagentRunsItsOwnModel(t *testing.T) {
	for _, tc := range []struct{ sub, want string }{{"deepseek/flash", "deepseek/flash"}, {"", "deepseek/pro"}} {
		model := &names{scripted: scripted{calls: [][2]string{{"explore", `{"input":"what is here?"}`}}}}
		o := withAgents(options(t, model), tc.sub)
		o.Model = "deepseek/pro"
		s, err := New(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Prompt(context.Background(), "explore"); err != nil {
			t.Fatal(err)
		}
		s.Close()
		model.mu.Lock()
		if len(model.parent) == 0 || len(model.child) == 0 {
			t.Fatalf("parent %v, child %v: want both asked", model.parent, model.child)
		}
		for _, n := range model.parent {
			if n != "deepseek/pro" {
				t.Errorf("parent asked %q", n)
			}
		}
		for _, n := range model.child {
			if n != tc.want {
				t.Errorf("subagent %q: child asked %q, want %q", tc.sub, n, tc.want)
			}
		}
		model.mu.Unlock()
	}
}

// sessions records the session ID on each model call's context, the
// main run's and the explorer's apart.
type sessions struct {
	scripted
	mu            sync.Mutex
	parent, child []string
}

func (m *sessions) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	m.mu.Lock()
	if strings.Contains(req.Instructions, "read-only explorer") {
		m.child = append(m.child, session.SessionIDFromContext(ctx))
	} else {
		m.parent = append(m.parent, session.SessionIDFromContext(ctx))
	}
	m.mu.Unlock()
	return m.scripted.CreateStream(ctx, req, sink)
}

// Each model call carries the ID of the session it is recorded in, so
// a session header names the main run's or the sub-agent's.
func TestEachModelCallCarriesItsSessionID(t *testing.T) {
	ctx := context.Background()
	model := &sessions{scripted: scripted{calls: [][2]string{{"explore", `{"input":"what is here?"}`}}}}
	o := withAgents(options(t, model), "")
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Prompt(ctx, "explore"); err != nil {
		t.Fatal(err)
	}
	sums, err := List(ctx, o.Root, o.Dir)
	if err != nil {
		t.Fatal(err)
	}
	var child string
	for _, sum := range sums {
		if sum.Header.ParentSession == s.ID() {
			child = sum.Header.ID
		}
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if len(model.parent) == 0 || len(model.child) == 0 || child == "" {
		t.Fatalf("parent calls %v, child calls %v, child session %q: want both", model.parent, model.child, child)
	}
	for _, id := range model.parent {
		if id != s.ID() {
			t.Errorf("main run's call carried %q, want %s", id, s.ID())
		}
	}
	for _, id := range model.child {
		if id != child {
			t.Errorf("explorer's call carried %q, want its own session %s", id, child)
		}
	}
}

func TestAChildSessionIsListedAndAJSONLSessionImports(t *testing.T) {
	ctx := context.Background()
	model := &scripted{calls: [][2]string{{"explore", `{"input":"what is here?"}`}}}
	o := withAgents(options(t, model), "")
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prompt(ctx, "explore"); err != nil {
		t.Fatal(err)
	}
	parent := s.ID()
	data := projected(t, o, s)

	sums, err := List(ctx, o.Root, o.Dir)
	if err != nil {
		t.Fatal(err)
	}
	var child string
	for _, sum := range sums {
		if sum.Header.ParentSession == parent {
			child = sum.Header.ID
		}
	}
	if len(sums) != 2 || child == "" {
		t.Fatalf("listed %d session(s), child %q; want the parent and its child", len(sums), child)
	}
	for _, id := range []string{parent, child} {
		if n, failed, err := Verify(ctx, o.Root, id); err != nil || len(failed) > 0 || n == 0 {
			t.Fatalf("verify %s: %d responses, failed %v, err %v", id, n, failed, err)
		}
	}

	// The projection of the parent imports into another store as its
	// record, and verifies there.
	file := filepath.Join(t.TempDir(), "old.jsonl")
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "store")
	id, err := Import(ctx, other, file)
	if err != nil || id != parent {
		t.Fatalf("import: %q, %v; want %s", id, err, parent)
	}
	if n, failed, err := Verify(ctx, other, id); err != nil || len(failed) > 0 || n == 0 {
		t.Fatalf("verify imported: %d responses, failed %v, err %v", n, failed, err)
	}
}

func TestAbsentSkillDirectoriesOfferNoSkillTool(t *testing.T) {
	o := options(t, &echo.Adapter{})
	if err := os.RemoveAll(filepath.Join(o.UserDir, "skills")); err != nil {
		t.Fatal(err)
	}
	s, err := New(context.Background(), o)
	if err != nil {
		t.Fatalf("New with no skill directory: %v", err)
	}
	defer s.Close()
	for _, tl := range s.Tools() {
		if tl.Name == "skill" {
			t.Errorf("the skill tool is offered with no skill directory")
		}
	}
}

func TestAHeldSessionIsReadWhileItIsWritten(t *testing.T) {
	ctx := context.Background()
	o := options(t, &echo.Adapter{})
	o.Sync = cas.SyncOnResponse
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, p := range []string{"one", "two"} {
		if _, err := s.Prompt(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	// The session is still held by s: list, verify and project read it
	// without its lock.
	sums, err := List(ctx, o.Root, o.Dir)
	if err != nil || len(sums) != 1 || sums[0].Size == 0 {
		t.Fatalf("list: %+v, %v; want one session with its size", sums, err)
	}
	before, failed, err := Verify(ctx, o.Root, s.ID())
	if err != nil || len(failed) > 0 || before == 0 {
		t.Fatalf("verify: %d responses, failed %v, err %v", before, failed, err)
	}
	if _, err := Project(ctx, o.Root, s.ID(), t.TempDir()); err != nil {
		t.Fatalf("project: %v", err)
	}
	// Packing beside the writer moves the objects, and the session
	// still appends and verifies.
	if n, err := GC(ctx, o.Root, false, time.Hour); err != nil || n == 0 {
		t.Fatalf("pack: %d, %v", n, err)
	}
	if _, err := s.Prompt(ctx, "three"); err != nil {
		t.Fatal(err)
	}
	if n, failed, err := Verify(ctx, o.Root, s.ID()); err != nil || len(failed) > 0 || n <= before {
		t.Fatalf("verify after pack: %d responses, failed %v, err %v; want more than %d", n, failed, err, before)
	}
}

func TestACallHeldWhenTheSessionStoppedIsAskedAgain(t *testing.T) {
	for _, approve := range []bool{true, false} {
		t.Run(map[bool]string{true: "approve", false: "deny"}[approve], func(t *testing.T) {
			ctx := context.Background()
			model := &scripted{calls: [][2]string{{"bash", `{"command":"echo ran"}`}}}
			o := options(t, model)
			o.Policy = confirmPolicy(t)
			s, err := New(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			// The run stops on the question and the process with it,
			// before anyone answers.
			end, err := s.Agent.Prompt(ctx, openresponses.UserText("run it"))
			if err != nil || end.Reason != agentturn.ReasonInputRequired {
				t.Fatalf("first run: %v, %v; want input_required", end, err)
			}
			id := s.ID()
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}

			var asked []string
			o.Approve = func(c *openresponses.FunctionCall, reason string) bool {
				asked = append(asked, c.Name+": "+reason)
				return approve
			}
			model.calls = nil
			s, err = Resume(ctx, o, id)
			if err != nil {
				t.Fatal(err)
			}
			if p := s.Pending(); len(p) != 1 || !held(p[0]) {
				t.Fatalf("resumed pending %+v, want the held bash call", p)
			}
			if end, err = s.Prompt(ctx, "and then?"); err != nil || end.Reason != agentturn.ReasonDone {
				t.Fatalf("resumed run: %v, %v", end, err)
			}
			if len(asked) != 1 || !strings.HasPrefix(asked[0], "bash: ") {
				t.Fatalf("asked %v, want the held bash call asked about again", asked)
			}
			var out string
			user := false
			for _, it := range s.Agent.State().Transcript {
				switch v := it.(type) {
				case *openresponses.FunctionCallOutput:
					out = v.Output.Text
				case *openresponses.Message:
					user = user || v.Text() == "and then?"
				}
			}
			if approve != strings.Contains(out, "ran") || approve == (out == deniedOutput) || !user {
				t.Errorf("approve=%v: bash output %q, the message in the transcript %v", approve, out, user)
			}
			data := string(projected(t, o, s))
			if !strings.Contains(data, `"by":"human"`) {
				t.Errorf("the answer is not recorded as a person's")
			}
			if n, failed, err := Verify(ctx, o.Root, id); err != nil || len(failed) > 0 || n == 0 {
				t.Errorf("verify: %d responses, failed %v, err %v", n, failed, err)
			}
		})
	}
}

// delegating is scripted for the parent; the explore child, known by
// its instructions, runs one bash call and answers.
type delegating struct{ scripted }

func (m *delegating) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	if strings.Contains(req.Instructions, "read-only explorer") {
		child := scripted{calls: [][2]string{{"bash", `{"command":"echo from the child"}`}}}
		return child.CreateStream(ctx, req, sink)
	}
	return m.scripted.CreateStream(ctx, req, sink)
}

// agentkit v0.0.4 revokes every skill grant at the first call it
// decides in another conversation. The explore child records into a
// session of its own, but its calls are decided by its own hook, not
// the kit's engine, so a child run between a skill read and the call
// the skill allows leaves the grant standing.
func TestAChildRunLeavesTheParentsSkillGrant(t *testing.T) {
	ctx := context.Background()
	model := &delegating{scripted{calls: [][2]string{
		{"skill", `{"name":"greet"}`},
		{"explore", `{"task":"what is here?"}`},
		{"bash", `{"command":"echo hello"}`},
	}}}
	o := options(t, model)
	o = withAgents(trustSkills(o), "")
	o.Policy = confirmPolicy(t)
	var asked []string
	o.Approve = func(c *openresponses.FunctionCall, _ string) bool {
		asked = append(asked, c.Name+" "+c.Arguments)
		return true
	}
	var notes []string
	o.Log = func(format string, args ...any) { notes = append(notes, fmt.Sprintf(format, args...)) }
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prompt(ctx, "greet me after looking around"); err != nil {
		t.Fatal(err)
	}
	// The child's own bash call is the child's to ask about: the
	// parent's grant covers the parent's. The parent's call after it
	// runs under the grant, unasked.
	if len(asked) != 1 || !strings.Contains(asked[0], "echo from the child") {
		t.Fatalf("asked %v; want only the child's call, the parent's grant of Bash(echo:*) covering the parent's alone", asked)
	}
	data := string(projected(t, o, s))
	if strings.Contains(data, "revoked") || !strings.Contains(data, `"type":"link"`) {
		t.Errorf("want the child linked and no grant revoked; notes %q", notes)
	}
}

// localWorkspace is dir, created if need be, as a workspace.Local,
// closed with the test.
func localWorkspace(t *testing.T, dir string) *workspace.Local {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.NewLocal(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	return ws
}
