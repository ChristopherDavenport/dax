package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agentturn/compact"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/ext/plan"
	"github.com/ChristopherDavenport/dax/policy"
)

// foldModels is scripted, keeping the model each fold's summary, or
// with server set each server compaction, was asked of.
type foldModels struct {
	scripted
	mu     sync.Mutex
	models []string
}

func (m *foldModels) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	if compact.IsFold(ctx) {
		m.mu.Lock()
		m.models = append(m.models, req.Model)
		m.mu.Unlock()
	}
	return m.scripted.CreateStream(ctx, req, sink)
}

func (m *foldModels) asked() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.models)
}

// serverFolds is foldModels with a compaction endpoint (-compact-server).
type serverFolds struct{ foldModels }

func (m *serverFolds) Compact(_ context.Context, req openresponses.CompactRequest) (*openresponses.CompactResponse, error) {
	m.mu.Lock()
	m.models = append(m.models, req.Model)
	m.mu.Unlock()
	return &openresponses.CompactResponse{Output: openresponses.Items{&openresponses.Compaction{EncryptedContent: "folded"}}}, nil
}

// windows is an Options.WindowOf over a table; a model it lacks has no
// known window.
func windows(by map[string]int64) func(context.Context, string) (int64, string) {
	return func(_ context.Context, model string) (int64, string) {
		if w := by[model]; w > 0 {
			return w, "config"
		}
		return 0, ""
	}
}

// sizedOptions is o with the context a start on a model whose window is
// start gives it, the budget from the window or, with flag set, from
// -compact.
func sizedOptions(o Options, start int64, flag int, by map[string]int64) Options {
	o.WindowOf = windows(by)
	o.Window, o.WindowFrom = windows(by)(context.Background(), o.Model)
	o.Compact, o.CompactFrom = int(start*3/4), "window"
	if flag > 0 {
		o.Compact, o.CompactFrom = flag, "flag"
	}
	return o
}

// fill prompts s four times with text of about 2,500 tokens each,
// longer than the 8 KiB a fold keeps of the person's own messages.
func fill(t *testing.T, ctx context.Context, s *Session) {
	t.Helper()
	for i := range 4 {
		if _, err := promptOn(ctx, s, strings.Repeat(fmt.Sprintf("part %d ", i), 10000/7), nil); err != nil {
			t.Fatal(err)
		}
	}
}

// A switch to a model with a smaller window sizes a budget from the
// window again, and the next request folds under it, the summary asked
// of the new model, locally and through the server; a start whose
// window is unknown has its compaction built all the same, with no
// budget, which the switch sizes. Each change is one dax.context entry
// in the record (#207).
func TestASwitchOfModelResizesTheBudget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		start  int64
		server bool
	}{
		{"smaller window", 40000, false},
		{"window unknown at the start", 0, false},
		{"through the server", 40000, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			var m interface {
				openresponses.Streamer
				asked() []string
			} = &foldModels{}
			if tc.server {
				m = &serverFolds{}
			}
			o := sizedOptions(options(t, m), tc.start, 0, map[string]int64{"echo": tc.start, "small": 8000})
			o.CompactServer = tc.server
			var notes []string
			o.Log = func(format string, args ...any) { notes = append(notes, fmt.Sprintf(format, args...)) }
			s, err := New(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			fill(t, ctx, s)
			if got := m.asked(); len(got) != 0 {
				t.Fatalf("folded under the start's budget: %v", got)
			}
			if err := s.SetModel("small"); err != nil {
				t.Fatal(err)
			}
			if z := s.sizes(); z.Compact != 6000 || z.Window != 8000 {
				t.Errorf("after the switch: %+v, want a budget of 6000 from a window of 8000", z)
			}
			if !strings.Contains(strings.Join(notes, "\n"), "[compaction budget 6000 tokens") {
				t.Errorf("notes %q, want the new budget said", notes)
			}
			if _, err := promptOn(ctx, s, "next", nil); err != nil {
				t.Fatal(err)
			}
			if got := m.asked(); !slices.Equal(got, []string{"small"}) {
				t.Errorf("folds asked of %v, want one of small", got)
			}
			data := string(projected(t, o, s))
			if n := strings.Count(data, `"ns":"dax"`); n != 1 {
				t.Errorf("%d dax.context entries, want 1", n)
			}
			if !strings.Contains(data, `"compact_budget":6000`) {
				t.Error("the record lacks the new budget")
			}
		})
	}
}

// A budget the person set (-compact) is kept through a switch of
// model; the new window is recorded (#207).
func TestASwitchKeepsACompactBudgetSet(t *testing.T) {
	ctx := context.Background()
	m := &foldModels{}
	o := sizedOptions(options(t, m), 40000, 30000, map[string]int64{"echo": 40000, "small": 8000})
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	fill(t, ctx, s)
	if err := s.SetModel("small"); err != nil {
		t.Fatal(err)
	}
	if z := s.sizes(); z.Compact != 30000 || z.CompactFrom != "flag" || z.Window != 8000 {
		t.Errorf("after the switch: %+v, want -compact's 30000 and the window of 8000", z)
	}
	if _, err := promptOn(ctx, s, "next", nil); err != nil {
		t.Fatal(err)
	}
	if got := m.asked(); len(got) != 0 {
		t.Errorf("folded under -compact's budget: %v", got)
	}
	data := string(projected(t, o, s))
	if strings.Count(data, `"ns":"dax"`) != 1 || !strings.Contains(data, `"window":8000`) {
		t.Error("the record lacks one dax.context entry with the new window")
	}
}

// Entering plan mode with plan_model sizes the context for it, and
// leaving puts back the start model's (#207).
func TestPlanModelResizesTheBudget(t *testing.T) {
	ctx := context.Background()
	o := sizedOptions(options(t, &scripted{}), 40000, 0, map[string]int64{"echo": 40000, "plan-model": 8000})
	o.Policy = confirmPolicy(t)
	o.Extensions = append(slices.Clone(o.Extensions), plan.New())
	o.PlanModel = "plan-model"
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, want := range []struct {
		mode   string
		budget int
	}{{policy.ModePlan, 6000}, {policy.ModeDefault, 30000}} {
		if _, err := s.SetMode(ctx, want.mode); err != nil {
			t.Fatal(err)
		}
		if z := s.sizes(); z.Compact != want.budget {
			t.Errorf("in %s on %s: budget %d, want %d", want.mode, s.Model(), z.Compact, want.budget)
		}
	}
}

// A session resumed under settings other than those its record last
// had writes one dax.context entry; resumed under the same ones, none
// (#207).
func TestAResumeUnderOtherSettingsIsRecorded(t *testing.T) {
	ctx := context.Background()
	o := sizedOptions(options(t, &scripted{}), 40000, 0, map[string]int64{"echo": 40000})
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := promptOn(ctx, s, "hi", nil); err != nil {
		t.Fatal(err)
	}
	id := s.ID()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	other := o
	other.Compact, other.CompactFrom = 20000, "flag"
	for _, opts := range []Options{o, other, other} {
		r, err := Resume(ctx, opts, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
	}
	r, err := Resume(ctx, other, id)
	if err != nil {
		t.Fatal(err)
	}
	data := string(projected(t, other, r))
	if n := strings.Count(data, `"ns":"dax"`); n != 1 || !strings.Contains(data, `"compact_budget":20000`) {
		t.Errorf("%d dax.context entries, want one with the budget of 20000", n)
	}
}

// /new after a switch of model opens the new session under the context
// the switch sized, which its header records, with no entry for a
// change (#207).
func TestNewAfterASwitchStartsUnderItsContext(t *testing.T) {
	ctx := context.Background()
	o := sizedOptions(options(t, &scripted{}), 40000, 0, map[string]int64{"echo": 40000, "small": 8000})
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetModel("small"); err != nil {
		t.Fatal(err)
	}
	n, err := s.Reopen(ctx, "")
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	s.Close()
	if z := n.sizes(); z.Compact != 6000 || z.Window != 8000 {
		t.Errorf("the new session's context %+v, want small's", z)
	}
	if _, err := promptOn(ctx, n, "hi", nil); err != nil {
		t.Fatal(err)
	}
	data := string(projected(t, o, n))
	if strings.Contains(data, `"ns":"dax"`) || !strings.Contains(data, `"compact_budget":6000`) {
		t.Error("want the header to hold small's budget and no dax.context entry")
	}
}

// While A2A callers are served (PeerConfig handed out), they stay on
// the model the session started with, and the kit's compaction is one
// for them and the person: a switch applies the smaller of the two
// models' budgets, 0 counting as none. A switch to a larger window
// keeps the callers' budget; to a smaller one, the person's, so the
// callers fold earlier than their model needs (#207 review M1).
func TestASwitchWhileServingCallersKeepsTheSmallerBudget(t *testing.T) {
	ctx := context.Background()
	by := map[string]int64{"echo": 200000, "big": 1000000, "small": 32000}
	for _, tc := range []struct {
		model   string
		callers bool
		want    int
	}{
		{"big", false, 750000},
		{"big", true, 150000},
		{"small", true, 24000},
		{"unknown", true, 150000},
	} {
		o := sizedOptions(options(t, &scripted{}), 200000, 0, by)
		s, err := New(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		if tc.callers {
			s.PeerConfig()
		}
		if err := s.SetModel(tc.model); err != nil {
			t.Fatal(err)
		}
		if got := s.sizes().Compact; got != tc.want {
			t.Errorf("to %s, callers served %v: budget %d, want %d", tc.model, tc.callers, got, tc.want)
		}
		s.Close()
	}
}

// Under -compact-server on a provider without the endpoint, a start
// whose window is unknown builds no compaction; a switch to a model
// whose window is known then sizes no budget, says why, and records
// none (review L2).
func TestASwitchWithoutACompactionRecordsNoBudget(t *testing.T) {
	ctx := context.Background()
	o := sizedOptions(options(t, &scripted{}), 0, 0, map[string]int64{"small": 8000})
	o.CompactServer = true
	var notes []string
	o.Log = func(format string, args ...any) { notes = append(notes, fmt.Sprintf(format, args...)) }
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetModel("small"); err != nil {
		t.Fatal(err)
	}
	if z := s.sizes(); z.Compact != 0 || z.Window != 8000 {
		t.Errorf("after the switch: %+v, want no budget and the window of 8000", z)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "no compaction endpoint") {
		t.Errorf("notes %q, want the missing endpoint said", notes)
	}
	data := string(projected(t, o, s))
	if strings.Contains(data, `"compact_budget":6000`) || !strings.Contains(data, `"window":8000`) {
		t.Error("want the record to hold the new window and no budget")
	}
}
