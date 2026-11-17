package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/policy"
)

// samplingKeys are the spellings of a sampling parameter a request
// could carry beside the spec's fields: what the anthropic adapter
// would forward from Extra (top_k) or read under its slug.
var samplingKeys = []string{"temperature", "top_p", "top_k", "anthropic.temperature", "anthropic.top_p", "anthropic.top_k"}

// No request dax builds carries a sampling parameter (#212): the
// anthropic adapter refuses temperature, top_p and top_k at any value,
// since API version 2026-10-21 does, so one would fail every Claude
// call. The session's own turns, a task sub-agent's and a fold's
// summary all go out without them, and the config has no field for one.
func TestNoRequestCarriesSampling(t *testing.T) {
	ctx := context.Background()
	w := &wire{next: &taskModels{
		parent: scripted{calls: [][2]string{{"task", `{"input":"write hello.txt saying hi"}`}}},
		child:  scripted{calls: [][2]string{{"write", `{"path":"hello.txt","content":"hi\n"}`}}},
	}}
	o := withAgents(options(t, w), "")
	o.Compact = 1
	o.Policy = &policy.Settings{Builtin: true, Fallback: "allow"}
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"one", "two", "three"} {
		if _, err := promptOn(ctx, s, p, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var child, fold bool
	for _, r := range w.reqs {
		child = child || strings.Contains(r.Instructions, "You are a sub-agent of dax")
		fold = fold || r.Reasoning.Effort == openresponses.ReasoningEffortNone && len(r.Tools) == 0
		if r.Temperature != nil || r.TopP != nil {
			t.Errorf("a request to %s carries temperature or top_p (%q)", r.Model, r.Instructions[:min(len(r.Instructions), 40)])
		}
		for _, k := range samplingKeys {
			if v, ok := r.Extra[k]; ok {
				t.Errorf("a request to %s carries %s %v", r.Model, k, v)
			}
		}
	}
	if !child || !fold {
		t.Errorf("requests seen %d, a sub-agent's %v, a fold's %v: want both", len(w.reqs), child, fold)
	}
}
