package agent

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses/echo"

	"github.com/ChristopherDavenport/dax/policy"
)

// #13: /new opens a fresh session over what the person changed during
// this one: the server /mcp add started, /model, /think and the mode.
func TestReopenKeepsWhatThePersonChanged(t *testing.T) {
	t.Setenv("DAX_TEST_MCP_SERVER", "1")
	ctx := context.Background()
	o := options(t, &echo.Adapter{})
	o.Policy = confirmPolicy(t)
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := promptOn(ctx, s, "zebra-before", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddMCP(ctx, "added", os.Args[0]); err != nil {
		t.Fatal(err)
	}
	if err := s.SetModel("other"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetThink(true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetMode(ctx, policy.ModeAcceptEdits); err != nil {
		t.Fatal(err)
	}
	old := s.ID()

	n, err := s.Reopen(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	// The old session closes without the store the new one now holds.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if n.ID() == "" || n.ID() == old {
		t.Fatalf("new session %q, old %q: want a session of its own", n.ID(), old)
	}
	if !slices.Contains(toolNames(n), "mcp__added__leak") {
		t.Errorf("tools after /new: %v, want the server /mcp add started", toolNames(n))
	}
	i := slices.IndexFunc(n.Info().MCP, func(m MCPServerInfo) bool { return m.Name == "added" })
	if i < 0 || n.Info().MCP[i].Added == "" {
		t.Errorf("MCP after /new: %+v, want the added server, removable by its label", n.Info().MCP)
	} else if err := n.RemoveMCP(n.Info().MCP[i].Added); err != nil {
		t.Errorf("remove the added server: %v", err)
	}
	if m := n.Model(); m != "other" {
		t.Errorf("model %q, want other", m)
	}
	if think, _ := n.now(); !think {
		t.Error("reasoning off after /new, want /think on kept")
	}
	if mode, _ := n.Mode(); mode != policy.ModeAcceptEdits {
		t.Errorf("mode %q, want acceptEdits", mode)
	}
	if _, err := promptOn(ctx, n, "second", nil); err != nil {
		t.Fatal(err)
	}
	rec := string(projected(t, o, n))
	if !strings.Contains(rec, `"model":"other"`) {
		t.Errorf("the new session's record names no request to model other:\n%s", rec)
	}
	if strings.Contains(rec, "zebra-before") {
		t.Errorf("the new session holds the old conversation:\n%s", rec)
	}
	// The old session's record is whole, and still its own.
	if _, failed, err := Verify(ctx, o.Root, old); err != nil || len(failed) != 0 {
		t.Errorf("old session: failed %v, err %v", failed, err)
	}
}
