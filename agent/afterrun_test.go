package agent

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/extension"
)

// An extension's AfterRun hook is told about each run of the main agent
// once it ends: its calls and whether one reached an open world; the
// session waits for it before it closes. (#160 a)
func TestAnAfterRunHookIsToldWhatTheRunDid(t *testing.T) {
	ctx := context.Background()
	o := options(t, &scripted{calls: [][2]string{{"ls", `{}`}}})
	got := make(chan extension.Run, 4)
	o.Extensions = append(slices.Clone(o.Extensions), extension.Extension{
		Name: "watcher",
		AfterRun: func(extension.Env) (func(context.Context, extension.Run), error) {
			return func(_ context.Context, r extension.Run) { got <- r }, nil
		},
	})
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	end, err := s.Prompt(ctx, openresponses.UserText("look"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-got:
		if r.ID != end.RunID || r.End == nil || !slices.Equal(r.Calls, []string{"ls"}) || r.OpenWorld {
			t.Errorf("the hook was told %+v; want run %s with one ls call, not open-world", r, end.RunID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hook was not told about the run")
	}
	if !(&afterRuns{}).openWorld("mcp__web__fetch") {
		t.Error("an MCP server's tool is not counted open-world")
	}
}
