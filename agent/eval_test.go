package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/ChristopherDavenport/agenteval"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses/echo"
)

// runnerOf is an agenteval.Runner that runs the dax configuration o in
// store, through HeaderOf, SessionOptionsOf and Build (#144).
func runnerOf(store agentsession.Store, o Options) *agenteval.Runner {
	return &agenteval.Runner{
		Store:          store,
		Header:         func(agenteval.Task) agentsession.Header { return HeaderOf(o) },
		SessionOptions: func(agenteval.Task) []session.Option { return SessionOptionsOf(o) },
		Build: func(ctx context.Context, _ agenteval.Task, rec *session.Recorder) (agentturn.Config, func() error, error) {
			run := o
			run.Root, run.Recorder = "", rec
			return Build(ctx, run)
		},
	}
}

// A dax configuration runs under agenteval's Runner: the session's
// header is the one dax writes, the env entry is recorded, a sub-agent's
// session links to the run's, and Compare reads two such runs (#144).
func TestADaxConfigurationRunsUnderAnEvaluationRunner(t *testing.T) {
	ctx := context.Background()
	model := &scripted{calls: [][2]string{{"explore", `{"input":"what is here?"}`}}}
	o := withAgents(options(t, model), "")
	o.Root = ""
	store := agentsession.NewMemoryStore()
	suite := &agenteval.Suite{Name: "dax", Tasks: []agenteval.Task{{ID: "look", Instruction: "explore"}}}

	report, err := runnerOf(store, o).Run(ctx, suite)
	if err != nil {
		t.Fatal(err)
	}
	res := report.Results[0]
	if res.Err != nil || res.Reason != agentturn.ReasonDone {
		t.Fatalf("run: reason %q, err %v", res.Reason, res.Err)
	}
	s, err := store.Open(ctx, res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	h := s.Header()
	raw, ok := h.Extra[headerWorkspace]
	if !ok {
		t.Fatalf("header extras %v: want %s", h.Extra, headerWorkspace)
	}
	var w recordedWorkspace
	if err := json.Unmarshal(raw, &w); err != nil || w.Kind != "local" || w.Root != o.Dir {
		t.Fatalf("%s = %s (%v), want the local workspace at %s", headerWorkspace, raw, err, o.Dir)
	}
	if h.Harness == nil || h.Harness.Name != "dax" || h.CWD != o.Dir || h.Media != agentsession.MediaSidecar {
		t.Fatalf("header harness %+v, cwd %q, media %q: want dax's", h.Harness, h.CWD, h.Media)
	}
	env, parts := false, false
	for _, e := range s.Entries() {
		switch v := e.(type) {
		case *agentsession.EnvEntry:
			env = v.CWD == o.Dir
		case *agentsession.ConfigEntry:
			parts = parts || len(v.InstructionsParts) > 0
		}
	}
	if !env || !parts {
		t.Fatalf("env entry of the workspace %v, instructions as parts %v: want both", env, parts)
	}
	child, n := "", 0
	for sum, err := range store.List(ctx, agentsession.ListFilter{}) {
		if err != nil {
			t.Fatal(err)
		}
		n++
		if sum.Header.ParentSession == res.SessionID {
			child = sum.Header.ID
		}
	}
	if child == "" {
		t.Fatalf("%d session(s): want explore's linked to the run's %s", n, res.SessionID)
	}

	// Two configurations, compared: the second keeps the context
	// budget on, which the header records.
	other := o
	other.Compact, other.CompactFrom = 50000, "test"
	cmp, err := agenteval.Compare(ctx, suite, runnerOf(agentsession.NewMemoryStore(), o), runnerOf(agentsession.NewMemoryStore(), other))
	if err != nil {
		t.Fatal(err)
	}
	if len(cmp.Pairs) != 1 || cmp.Pairs[0].A.Err != nil || cmp.Pairs[0].B.Err != nil {
		t.Fatalf("compare: %+v", cmp.Pairs)
	}
}

// A header that is not the one dax writes is refused at Build, so an
// evaluation never records a dax run under another's header (#144).
func TestBuildRefusesAHeaderThatIsNotDaxs(t *testing.T) {
	ctx := context.Background()
	o := options(t, &echo.Adapter{})
	o.Root = ""
	store := agentsession.NewMemoryStore()
	r := runnerOf(store, o)
	r.Header = func(agenteval.Task) agentsession.Header {
		return agentsession.Header{CWD: filepath.Join(o.Dir, "elsewhere")}
	}
	report, err := r.Run(ctx, &agenteval.Suite{Name: "dax", Tasks: []agenteval.Task{{ID: "x", Instruction: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if report.Results[0].Err == nil {
		t.Fatal("Build took a header dax does not write")
	}
}
