package provider

import (
	"context"
	"strings"
	"testing"

	"golang.org/x/oauth2/google"

	"github.com/ChristopherDavenport/openresponses"
)

// recorder is a streamer that notes the models it was asked for.
type recorder struct{ got *[]string }

func (r recorder) CreateStream(_ context.Context, req openresponses.Request, _ openresponses.EventSink) error {
	*r.got = append(*r.got, req.Model)
	return nil
}

func TestVertexRoutesEachRequestByItsModel(t *testing.T) {
	var claude, gemini []string
	v := vertexModels{claude: recorder{&claude}, gemini: recorder{&gemini}}
	for _, model := range []string{"claude-opus-5-5", "gemini-3.5-flash", "claude-haiku-4-5", "gemini-2.5-pro"} {
		if err := v.CreateStream(context.Background(), openresponses.Request{Model: model}, nil); err != nil {
			t.Fatalf("%s: %v", model, err)
		}
	}
	if strings.Join(claude, ",") != "claude-opus-5-5,claude-haiku-4-5" || strings.Join(gemini, ",") != "gemini-3.5-flash,gemini-2.5-pro" {
		t.Errorf("claude got %v, gemini got %v", claude, gemini)
	}
	// A model of neither family reaches neither adapter: a /model
	// switch to one fails on its request, not somewhere else.
	for _, model := range []string{"", "gpt-5", "llama-4", "publishers/google/models/gemini-2.5-pro", "Claude-opus-5-5"} {
		err := v.CreateStream(context.Background(), openresponses.Request{Model: model}, nil)
		if err == nil || !strings.Contains(err.Error(), "not a claude- or gemini- model") {
			t.Errorf("%q: err = %v", model, err)
		}
	}
	if len(claude)+len(gemini) != 4 {
		t.Errorf("an unroutable request reached an adapter: %v %v", claude, gemini)
	}
	// Neither adapter compacts, and the router says so rather than
	// hiding the method.
	if _, err := v.Compact(context.Background(), openresponses.CompactRequest{Model: "claude-opus-5-5"}); err == nil {
		t.Error("compaction should be unsupported")
	}
}

func TestVertexFitsEffortsByGeneration(t *testing.T) {
	ctx := context.Background()
	d := vertexDescriber{}
	for _, tc := range []struct {
		model string
		// want maps a requested effort to the one sent; empty for a
		// model that is unknown, where every effort is sent as asked.
		want map[string]string
	}{
		{"claude-haiku-4-5", map[string]string{"high": "none", "low": "none", "none": "none"}},
		{"claude-opus-4-5", map[string]string{"xhigh": "none"}},
		{"claude-3-7-sonnet", map[string]string{"high": "none"}},
		{"claude-opus-4-6", map[string]string{"xhigh": "high", "high": "high", "none": "none"}},
		{"claude-sonnet-4-6", map[string]string{"xhigh": "high", "minimal": "minimal"}},
		{"claude-opus-4-7", nil},
		{"claude-sonnet-5", nil},
		{"claude-opus-5", nil},
		{"claude-opus-5-5", map[string]string{"none": "minimal", "xhigh": "xhigh", "high": "high"}},
		{"claude-sonnet-5-5", map[string]string{"none": "minimal", "low": "low"}},
		{"claude-opus-6", map[string]string{"none": "minimal"}},
		{"claude-sonnet-5-5-20261001", map[string]string{"none": "minimal"}},
		{"claude-next", nil},
		{"gemini-2.5-pro", map[string]string{"none": "low", "minimal": "low", "medium": "medium", "xhigh": "high"}},
		{"gemini-3.8-flash", map[string]string{"none": "low", "minimal": "low", "high": "high"}},
	} {
		info, err := d.Describe(ctx, tc.model)
		if err != nil {
			t.Fatalf("%s: %v", tc.model, err)
		}
		if tc.want == nil {
			if info.Known() {
				t.Errorf("%s: described as %v, want unknown", tc.model, info)
			}
			continue
		}
		for in, out := range tc.want {
			if got := info.Fit(openresponses.ReasoningEffort(in)); string(got) != out {
				t.Errorf("%s: %s fits to %s, want %s", tc.model, in, got, out)
			}
		}
	}
	if _, err := d.Describe(ctx, "gpt-5"); err == nil {
		t.Error("an unroutable model should not be described")
	}
}

func TestClaudeGeneration(t *testing.T) {
	for model, want := range map[string][3]int{
		"claude-opus-5-5":            {5, 5, 1},
		"claude-sonnet-5":            {5, 0, 1},
		"claude-haiku-4-5":           {4, 5, 1},
		"claude-3-7-sonnet":          {3, 7, 1},
		"claude-3-5-sonnet-20241022": {3, 5, 1},
		"claude-sonnet-4-20250514":   {4, 0, 1},
		"claude-fable":               {0, 0, 0},
	} {
		major, minor, ok := claudeGeneration(model)
		if major != want[0] || minor != want[1] || ok != (want[2] == 1) {
			t.Errorf("%s: %d.%d %v, want %v", model, major, minor, ok, want)
		}
	}
}

func TestVertexRefusesAModelOfNeitherFamilyBeforeCredentials(t *testing.T) {
	asked := false
	spy := func(ctx context.Context) (*google.Credentials, error) { asked = true; return fakeCredentials(ctx) }
	vars := env(map[string]string{ProjectEnv: "p", LocationEnv: "global"})
	for _, spec := range []Spec{
		{Provider: "vertex", Model: "gpt-5"},
		{Provider: "vertex", Model: "claude-opus-5-5", SubagentModel: "llama-4"},
	} {
		spec.Getenv, spec.Credentials = vars, spy
		if _, err := New(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "not a claude- or gemini- model") {
			t.Errorf("%+v: err = %v", spec, err)
		}
	}
	if asked {
		t.Error("credentials were looked for with a model that cannot work")
	}
	// The families mix: Claude for the main agent, Gemini for the
	// sub-agents, and the other way round.
	for _, spec := range []Spec{
		{Provider: "vertex", Model: "claude-opus-5-5", SubagentModel: "gemini-3.5-flash"},
		{Provider: "vertex", Model: "gemini-3.1-pro-preview", SubagentModel: "claude-sonnet-5-5"},
	} {
		spec.Getenv, spec.Credentials = vars, fakeCredentials
		m, err := New(context.Background(), spec)
		if err != nil || m.Name != spec.Model || m.SubagentName != spec.SubagentModel {
			t.Errorf("%+v: %+v, %v", spec, m, err)
		}
	}
}
