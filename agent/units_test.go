package agent

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// A box host's MCP units are the session's MCP servers, each started
// through the executor by the reserved word the box host answers; its
// workspace unit is not one.
func TestUnitServersOf(t *testing.T) {
	raw := json.RawMessage(`{"profile":"go","units":[
		{"name":"workspace","kind":"workspace","files":"project rw","internet":"off"},
		{"name":"files","kind":"mcp","files":"project ro","internet":"off"},
		{"name":"gh","kind":"mcp","files":"none","internet":"allow","allow":["api.github.com"],"secrets":["GITHUB_TOKEN"]}]}`)
	got, err := unitServersOf(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []MCPServer{{Name: "files", Command: "dax-unit files"}, {Name: "gh", Command: "dax-unit gh"}}
	if !slices.Equal(got, want) {
		t.Errorf("unitServersOf = %+v, want %+v", got, want)
	}
	if got, err := unitServersOf(nil); err != nil || got != nil {
		t.Errorf("no box host = %+v, %v", got, err)
	}
	for _, bad := range []string{`{"units":[{"name":"x y","kind":"mcp"}]}`, `{"units":[{"name":"a__b","kind":"mcp"}]}`, `not json`} {
		if _, err := unitServersOf(json.RawMessage(bad)); err == nil {
			t.Errorf("%s was taken", bad)
		}
	}
}

func TestWithUnits(t *testing.T) {
	mine := []MCPServer{{Name: "cfg", Command: "server"}}
	units := []MCPServer{{Name: "files", Command: "dax-unit files"}}
	got, err := withUnits(mine, units)
	if err != nil || !slices.Equal(got, append(slices.Clone(mine), units...)) {
		t.Errorf("withUnits = %+v, %v", got, err)
	}
	if len(mine) != 1 {
		t.Error("the session's own servers were changed")
	}
	if _, err := withUnits([]MCPServer{{Name: "files", Command: "x"}}, units); err == nil || !strings.Contains(err.Error(), "rename the server") {
		t.Errorf("a server and a unit of one name: %v", err)
	}
}
