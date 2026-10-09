package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/ChristopherDavenport/openresponses/echo"

	"github.com/ChristopherDavenport/dax/internal/modelinfo"
	"github.com/ChristopherDavenport/dax/policy"
)

// wire keeps the hash of every request as it reached the model, below
// every layer dax puts between the loop and the provider.
type wire struct {
	next    openresponses.Streamer
	mu      sync.Mutex
	hashes  []string
	reqs    []openresponses.Request
	efforts map[string][]openresponses.ReasoningEffort // by model
}

// taskModels is a scripted parent and a scripted task child, the child
// told apart by the task preamble, keeping the child's requests.
type taskModels struct {
	parent, child scripted
	mu            sync.Mutex
	childReqs     []openresponses.Request
}

func (m *taskModels) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	if strings.Contains(req.Instructions, "You are a sub-agent of dax") {
		m.mu.Lock()
		m.childReqs = append(m.childReqs, req)
		m.mu.Unlock()
		return m.child.CreateStream(ctx, req, sink)
	}
	return m.parent.CreateStream(ctx, req, sink)
}

func (w *wire) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	// The format hashes the request less its transport members.
	h, err := session.RequestHash(session.Canonical(req))
	if err != nil {
		return err
	}
	w.mu.Lock()
	w.hashes = append(w.hashes, h)
	w.reqs = append(w.reqs, req)
	if w.efforts == nil {
		w.efforts = map[string][]openresponses.ReasoningEffort{}
	}
	w.efforts[req.Model] = append(w.efforts[req.Model], req.Reasoning.Effort)
	w.mu.Unlock()
	return w.next.CreateStream(ctx, req, sink)
}

// efforts describes the main model as taking no reasoning and the
// sub-agent model as taking only none and high, so -think's low is
// changed for both.
type efforts map[string][]openresponses.ReasoningEffort

func (e efforts) Describe(_ context.Context, model string) (modelinfo.Info, error) {
	return modelinfo.Info{Source: "test", Efforts: e[model]}, nil
}

// recorded is the request_hash of every response entry of a session.
func recorded(t *testing.T, o Options, id string) []string {
	t.Helper()
	path, err := Project(context.Background(), o.Root, id, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(nil, 1<<24)
	for sc.Scan() {
		var e struct {
			Type        string `json:"type"`
			RequestHash string `json:"request_hash"`
		}
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		if e.Type == "response" {
			out = append(out, e.RequestHash)
		}
	}
	return out
}

// What a session records as sent must be what was sent: every request
// hash in the parent's and the task child's sessions is the hash of a
// request the model received, and every request the model received is
// recorded. The effort fitting and the task's instructions both
// changed requests below the recorder, which -verify cannot see, since
// it checks the record against itself.
func TestTheRecordIsWhatWasSent(t *testing.T) {
	for _, tc := range []struct {
		name, args string
		// efforts is what each model must have been asked for: -think's
		// low fitted to the main model (none only) and to the sub-agent
		// model (none, high).
		efforts map[string]openresponses.ReasoningEffort
		fork    bool
	}{
		{"fresh on the sub-agent model", `{"input":"write hello.txt saying hi"}`, map[string]openresponses.ReasoningEffort{"pro": "none", "flash": "high"}, false},
		{"fork on the main model", `{"input":"write hello.txt saying hi","context":"fork","model":"main"}`, map[string]openresponses.ReasoningEffort{"pro": "none"}, true},
		{"fork on the sub-agent model", `{"input":"write hello.txt saying hi","context":"fork"}`, map[string]openresponses.ReasoningEffort{"pro": "none", "flash": "high"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			w := &wire{next: &taskModels{
				parent: scripted{calls: [][2]string{{"task", tc.args}}},
				child:  scripted{calls: [][2]string{{"write", `{"path":"hello.txt","content":"hi\n"}`}}},
			}}
			o := options(t, nil)
			o.Streamer = modelinfo.Wrap(w, efforts{"pro": {"none"}, "flash": {"none", "high"}}, nil)
			o.Fit = modelinfo.Of(o.Streamer).Fit
			o = withAgents(o, "flash")
			o.Model, o.Think = "pro", true
			o.Instructions = "Use tabs."
			o.Policy = &policy.Settings{Builtin: true, Fallback: "allow"}
			s, err := New(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := promptOn(ctx, s, "go on with the plan we settled", nil); err != nil {
				t.Fatal(err)
			}
			parent := s.ID()
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			sums, err := List(ctx, o.Root, o.Dir)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			children := 0
			for _, sum := range sums {
				if sum.Header.ID == parent || sum.Header.ParentSession == parent {
					got = append(got, recorded(t, o, sum.Header.ID)...)
				}
				if sum.Header.ParentSession == parent {
					children++
				}
			}
			if children != 1 {
				t.Fatalf("%d child sessions, want the task's", children)
			}
			w.mu.Lock()
			defer w.mu.Unlock()
			sent := slices.Clone(w.hashes)
			slices.Sort(got)
			slices.Sort(sent)
			if !slices.Equal(got, sent) {
				t.Errorf("recorded request hashes differ from the requests sent:\nrecorded %v\nsent     %v", got, sent)
			}
			for model, want := range tc.efforts {
				if len(w.efforts[model]) == 0 {
					t.Errorf("%s was never asked", model)
				}
				for _, e := range w.efforts[model] {
					if e != want {
						t.Errorf("%s was asked for effort %q, want %q", model, e, want)
					}
				}
			}
			for model := range w.efforts {
				if _, ok := tc.efforts[model]; !ok {
					t.Errorf("%s was asked, want only %v", model, tc.efforts)
				}
			}
			// The child's first request: a fork opens with the main
			// agent's conversation, the user's message and the task call
			// answered with a placeholder, then the brief; a fresh task
			// with the brief alone.
			var first *openresponses.Request
			for i := range w.reqs {
				if strings.Contains(w.reqs[i].Instructions, "You are a sub-agent of dax") {
					first = &w.reqs[i]
					break
				}
			}
			if first == nil {
				t.Fatal("no child request")
			}
			seesParent := strings.Contains(itemsText(first.Input), "go on with the plan we settled")
			if seesParent != tc.fork {
				t.Errorf("the child sees the main agent's conversation: %v, want %v", seesParent, tc.fork)
			}
			if tc.fork && !strings.Contains(itemsText(first.Input), "Everything above is the main agent's conversation") {
				t.Error("a fork's brief lacks its directive")
			}
		})
	}
}

// itemsText is the text of every message in items.
func itemsText(items openresponses.Items) string {
	var b strings.Builder
	for _, it := range items {
		if m, ok := it.(*openresponses.Message); ok {
			b.WriteString(m.Text())
			b.WriteString("\n")
		}
	}
	return b.String()
}

// A fold's summary request is asked at effort none, fitted to the model
// like any other: on one that always reasons it goes out at its least
// effort, and the compaction entry's configuration says the same.
func TestAFoldRecordsTheEffortItSent(t *testing.T) {
	ctx := context.Background()
	w := &wire{next: &echo.Adapter{}}
	o := options(t, nil)
	o.Streamer = modelinfo.Wrap(w, efforts{"pro": {"low", "high"}}, nil)
	o.Fit = modelinfo.Of(o.Streamer).Fit
	o.Model, o.Compact = "pro", 1
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"one", "two", "three", "four"} {
		if _, err := promptOn(ctx, s, p, nil); err != nil {
			t.Fatal(err)
		}
	}
	w.mu.Lock()
	for _, e := range w.efforts["pro"] {
		if e != "low" {
			t.Errorf("a request asked for %q, want every one fitted to low", e)
		}
	}
	w.mu.Unlock()
	data := projected(t, o, s)
	folds := 0
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(nil, 1<<24)
	for sc.Scan() {
		var e struct {
			Type   string `json:"type"`
			Config struct {
				Reasoning struct {
					Effort string `json:"effort"`
				} `json:"reasoning"`
			} `json:"config"`
		}
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		if e.Type == "compaction" {
			folds++
			if e.Config.Reasoning.Effort != "low" {
				t.Errorf("a fold records effort %q, want low, what was sent", e.Config.Reasoning.Effort)
			}
		}
	}
	if folds == 0 {
		t.Fatal("nothing was folded")
	}
}

// /think and /model fit the effort to the model in force, and a model
// switch keeps what /think set.
func TestThinkAndModelSwitchesAreFitted(t *testing.T) {
	ctx := context.Background()
	o := options(t, &scripted{})
	o.Streamer = modelinfo.Wrap(&scripted{}, efforts{"pro": {"none"}, "always": {"low", "high"}}, nil)
	o.Fit = modelinfo.Of(o.Streamer).Fit
	o.Model, o.Think = "pro", true
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	effort := func() openresponses.ReasoningEffort { return s.ag.Config().Reasoning.Effort }
	if e := effort(); e != "none" {
		t.Errorf("start on pro under -think: %q, want none", e)
	}
	if err := s.SetModel("always"); err != nil {
		t.Fatal(err)
	}
	if e := effort(); e != "low" {
		t.Errorf("switched to a model that always reasons: %q, want low", e)
	}
	if err := s.SetThink(false); err != nil {
		t.Fatal(err)
	}
	if e := effort(); e != "low" {
		t.Errorf("/think off on a model that always reasons: %q, want its least, low", e)
	}
	if err := s.SetModel("free"); err != nil {
		t.Fatal(err)
	}
	if r := s.ag.Config().Reasoning; r.Effort != "none" || r.Summary != "" {
		t.Errorf("after /think off, a switch to an unknown model: %+v, want none kept", r)
	}
}

// The configured effort is what -think asks for, fitted to each model,
// and /think on brings it back after /think off.
func TestTheConfiguredEffortIsAskedAndFitted(t *testing.T) {
	ctx := context.Background()
	o := options(t, &scripted{})
	o.Streamer = modelinfo.Wrap(&scripted{}, efforts{"glm": {"low", "high"}, "mid": {"none", "low", "medium"}}, nil)
	o.Fit = modelinfo.Of(o.Streamer).Fit
	o.Model, o.Think, o.Effort = "glm", true, openresponses.ReasoningEffortHigh
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	effort := func() openresponses.ReasoningEffort { return s.ag.Config().Reasoning.Effort }
	if e := effort(); e != "high" {
		t.Errorf("start on glm with effort high: %q, want high", e)
	}
	if err := s.SetModel("mid"); err != nil {
		t.Fatal(err)
	}
	if e := effort(); e != "medium" {
		t.Errorf("switched to a model whose most is medium: %q, want medium", e)
	}
	if err := s.SetThink(false); err != nil {
		t.Fatal(err)
	}
	if e := effort(); e != "none" {
		t.Errorf("/think off: %q, want none", e)
	}
	if err := s.SetModel("glm"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetThink(true); err != nil {
		t.Fatal(err)
	}
	if e := effort(); e != "high" {
		t.Errorf("/think on again: %q, want the configured high", e)
	}
}

// switchable is a scripted parent whose next call can be changed
// between runs, over a wire that keeps every child request.
type switchable struct {
	mu     sync.Mutex
	parent scripted
	child  []openresponses.Request
}

func (m *switchable) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	m.mu.Lock()
	sub := strings.Contains(req.Instructions, "You are a sub-agent of dax") || strings.Contains(req.Instructions, "read-only explorer")
	if sub {
		m.child = append(m.child, req)
	}
	parent := m.parent
	m.mu.Unlock()
	if sub {
		return (&scripted{}).CreateStream(ctx, req, sink)
	}
	return parent.CreateStream(ctx, req, sink)
}

func (m *switchable) next(name, args string) {
	m.mu.Lock()
	m.parent = scripted{calls: [][2]string{{name, args}}}
	m.child = nil
	m.mu.Unlock()
}

func (m *switchable) last() openresponses.Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.child[0]
}

// /think and /model reach the sub-agents from their next call: the
// effort each asks for, the model a task with model main runs, and,
// with no sub-agent model configured, the model every sub-agent runs.
func TestSubagentsFollowThinkAndModel(t *testing.T) {
	ctx := context.Background()
	m := &switchable{}
	o := options(t, nil)
	o.Streamer = modelinfo.Wrap(m, efforts{"pro": {"none", "low"}, "other": {"none", "low"}}, nil)
	o.Fit = modelinfo.Of(o.Streamer).Fit
	o = withAgents(o, "")
	o.Model, o.Think = "pro", true
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	run := func(tool, args string) openresponses.Request {
		t.Helper()
		m.next(tool, args)
		if _, err := promptOn(ctx, s, "go", nil); err != nil {
			t.Fatal(err)
		}
		return m.last()
	}
	if r := run("task", `{"input":"x"}`); r.Model != "pro" || r.Reasoning.Effort != "low" {
		t.Errorf("before: task asked %s at %q, want pro at low", r.Model, r.Reasoning.Effort)
	}
	if err := s.SetThink(false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetModel("other"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ tool, args string }{
		{"task", `{"input":"x","model":"main"}`},
		{"task", `{"input":"x"}`},
		{"explore", `{"input":"x"}`},
	} {
		if r := run(c.tool, c.args); r.Model != "other" || r.Reasoning.Effort != "none" {
			t.Errorf("after /think off and /model other: %s %s asked %s at %q, want other at none", c.tool, c.args, r.Model, r.Reasoning.Effort)
		}
	}
	// The main agent's own setting holds too.
	if r := s.ag.Config().Reasoning; r.Effort != "none" {
		t.Errorf("main agent effort %q", r.Effort)
	}
}
