package modelinfo

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
)

// recorder is a model that keeps each request it is sent.
type recorder struct {
	mu   sync.Mutex
	reqs []openresponses.Request
}

func (r *recorder) CreateStream(_ context.Context, req openresponses.Request, _ openresponses.EventSink) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, req)
	return nil
}

func (r *recorder) last() openresponses.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reqs[len(r.reqs)-1]
}

type compactingRecorder struct{ recorder }

func (c *compactingRecorder) Compact(context.Context, openresponses.CompactRequest) (*openresponses.CompactResponse, error) {
	return &openresponses.CompactResponse{}, nil
}

// table describes from a map and counts the calls.
type table struct {
	mu    sync.Mutex
	infos map[string]Info
	err   error
	calls int
}

func (d *table) Describe(ctx context.Context, model string) (Info, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	if d.err != nil {
		return Info{}, d.err
	}
	info, ok := d.infos[model]
	if !ok {
		return Info{}, errors.New("no such model")
	}
	return info, nil
}

func send(t *testing.T, s openresponses.Streamer, model string, r openresponses.ReasoningConfig) {
	t.Helper()
	if err := s.CreateStream(context.Background(), openresponses.Request{Model: model, Reasoning: r}, nil); err != nil {
		t.Fatal(err)
	}
}

var (
	low  = openresponses.ReasoningConfig{Effort: "low", Summary: openresponses.ReasoningSummaryAuto}
	off  = openresponses.ReasoningConfig{Effort: "none"}
	vary = &table{infos: map[string]Info{
		"always": {Source: "openrouter", Reasons: Yes, Efforts: []E{"low", "high"}},
		"never":  {Source: "ollama", Reasons: No, Efforts: []E{"none"}},
		"thinks": {Source: "ollama", Reasons: Yes},
	}}
)

func TestFitFitsAndTellsOnce(t *testing.T) {
	var notes []string
	s := Of(Wrap(&recorder{}, vary, func(m string) { notes = append(notes, m) }))
	ctx := context.Background()
	for _, tc := range []struct {
		model     string
		want, got E
	}{
		{"always", "none", "low"},
		{"never", "low", "none"},
		{"thinks", "low", "low"},
		{"always", "", ""},
		{"always", "none", "low"},
		{"never", "low", "none"},
	} {
		if got := s.Fit(ctx, tc.model, tc.want); got != tc.got {
			t.Errorf("Fit(%s, %q) = %q, want %q", tc.model, tc.want, got, tc.got)
		}
	}
	if len(notes) != 2 || !strings.Contains(notes[0], "always: reasoning effort none is not accepted; asking for low (openrouter)") ||
		!strings.Contains(notes[1], "never: reasoning effort low is not accepted; asking for none (ollama)") {
		t.Errorf("notes: %q", notes)
	}
}

// A session records the request the loop built, so the Streamer, below
// the recorder, must send it unchanged; a request that was not fitted
// is reported so the path that built it can be found.
func TestRequestsAreSentAsTheyAre(t *testing.T) {
	var notes []string
	inner := &recorder{}
	s := Wrap(inner, vary, func(m string) { notes = append(notes, m) })
	send(t, s, "never", low)
	send(t, s, "never", low)
	send(t, s, "always", off)
	if got := inner.last().Reasoning; got != off {
		t.Errorf("changed: %+v", got)
	}
	if got := inner.reqs[0].Reasoning; got != low {
		t.Errorf("changed: %+v", got)
	}
	if len(notes) != 2 || !strings.Contains(notes[0], "never: a request asks for reasoning effort low, which ollama says is not accepted; sent as asked") {
		t.Errorf("notes: %q", notes)
	}
	// A fitted request goes without a note.
	notes = nil
	send(t, s, "never", off)
	if len(notes) != 0 {
		t.Errorf("a fitted request noted: %q", notes)
	}
}

func TestEachModelIsAskedOnce(t *testing.T) {
	d := &table{infos: vary.infos}
	s := Wrap(&recorder{}, d, nil)
	for range 3 {
		send(t, s, "always", low)
		send(t, s, "never", low)
	}
	if _, err := Of(s).Describe(context.Background(), "always"); err != nil {
		t.Fatal(err)
	}
	if d.calls != 2 {
		t.Errorf("described %d times, want once per model", d.calls)
	}
}

func TestAFailureIsKeptAndToldOnce(t *testing.T) {
	d := &table{err: errors.New("catalogue down")}
	var notes []string
	s := Of(Wrap(&recorder{}, d, func(m string) { notes = append(notes, m) }))
	for range 2 {
		if got := s.Fit(context.Background(), "m", "low"); got != "low" {
			t.Errorf("asked as configured: %q", got)
		}
	}
	if d.calls != 1 || len(notes) != 1 || !strings.Contains(notes[0], "model info for m unavailable: catalogue down") {
		t.Errorf("calls %d, notes %q", d.calls, notes)
	}
}

func TestACancelledDescribeIsNotKept(t *testing.T) {
	d := &table{infos: vary.infos}
	var notes []string
	s := Wrap(&recorder{}, d, func(m string) { notes = append(notes, m) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = Of(s).Fit(ctx, "never", "low")
	if len(notes) != 0 {
		t.Errorf("a cancelled request told: %q", notes)
	}
	info, err := Of(s).Describe(context.Background(), "never")
	if err != nil || info.Reasons != No || d.calls != 2 {
		t.Errorf("after a cancelled describe: %+v %v, calls %d", info, err, d.calls)
	}
}

func TestNoDescriberSendsAsAsked(t *testing.T) {
	inner := &recorder{}
	s := Wrap(inner, nil, nil)
	send(t, s, "anything", off)
	if got := inner.last().Reasoning; got != off {
		t.Errorf("%+v", got)
	}
	if info, err := Of(s).Describe(context.Background(), "anything"); err != nil || info.Known() {
		t.Errorf("%+v %v", info, err)
	}
}

func TestCompactionIsKeptExactly(t *testing.T) {
	type compacts interface {
		Compact(context.Context, openresponses.CompactRequest) (*openresponses.CompactResponse, error)
	}
	if _, ok := Wrap(&recorder{}, nil, nil).(compacts); ok {
		t.Error("a model that does not compact gained Compact")
	}
	w := Wrap(&compactingRecorder{}, nil, nil)
	c, ok := w.(compacts)
	if !ok {
		t.Fatal("a model that compacts lost Compact")
	}
	if _, err := c.Compact(context.Background(), openresponses.CompactRequest{}); err != nil {
		t.Error(err)
	}
	if Of(w) == nil || Of(&recorder{}) != nil {
		t.Error("Of")
	}
}
