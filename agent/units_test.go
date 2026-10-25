package agent

import (
	"encoding/json"
	"slices"
	"testing"
)

// The banner names a box host's MCP units, not its workspace unit; a
// box host's answer it cannot read names none.
func TestBoxUnits(t *testing.T) {
	raw := json.RawMessage(`{"profile":"go","units":[
		{"name":"workspace","kind":"workspace","files":"project rw","internet":"off"},
		{"name":"files","kind":"mcp","files":"project ro","internet":"off"},
		{"name":"gh","kind":"mcp","files":"none","internet":"allow","allow":["api.github.com"],"secrets":["GITHUB_TOKEN"]}]}`)
	if got := boxUnits(raw); !slices.Equal(got, []string{"files", "gh"}) {
		t.Errorf("boxUnits = %v", got)
	}
	for _, none := range []json.RawMessage{nil, json.RawMessage(`not json`)} {
		if got := boxUnits(none); got != nil {
			t.Errorf("boxUnits(%s) = %v, want none", none, got)
		}
	}
}
