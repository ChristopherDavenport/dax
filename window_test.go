package dax

import (
	"context"
	"testing"

	"github.com/ChristopherDavenport/dax/internal/modelinfo"
)

// A switch of model takes the window of the model it switches to: the
// start model's is the one dax settled on, -context-window included;
// another's is the user's context_window for it, lowered by the
// project's, and unknown without either or a vendor (#207).
func TestASwitchTakesTheNewModelsWindow(t *testing.T) {
	start := modelinfo.Info{Source: "anthropic", ContextWindow: 200000}.WithWindow(0, 0, 50000)
	of := windowsByModel(nil, "big", start, map[string]int64{"small": 32000, "mid": 64000}, map[string]int64{"mid": 16000})
	for _, tc := range []struct {
		model  string
		window int64
		from   string
	}{
		{"big", 50000, "flag"},
		{"small", 32000, "config"},
		{"mid", 16000, "project config"},
		{"other", 0, ""},
	} {
		if w, from := of(context.Background(), tc.model); w != tc.window || from != tc.from {
			t.Errorf("%s: window %d from %q, want %d from %q", tc.model, w, from, tc.window, tc.from)
		}
	}
	if w, from := windowOf(modelinfo.Info{Source: "openrouter", ContextWindow: 128000}); w != 128000 || from != "vendor:openrouter" {
		t.Errorf("a vendor's window: %d from %q", w, from)
	}
}
