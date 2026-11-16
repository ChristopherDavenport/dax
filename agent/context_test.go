package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ChristopherDavenport/openresponses/echo"

	"github.com/ChristopherDavenport/dax/internal/config"
)

// The header says what context strategy the session ran: its budget
// and the window dax believed, so two sessions that differ only in
// -compact compare as different (#174), and, in a member of its own,
// where each came from.
func TestTheHeaderRecordsTheContextSettings(t *testing.T) {
	ctx := context.Background()
	type both struct {
		recordedContext
		recordedContextFrom
	}
	got := map[string]both{}
	for name, set := range map[string]func(*Options){
		"flag": func(o *Options) {
			o.Compact, o.CompactFrom, o.Window, o.WindowFrom = 1000, "flag", 65536, "ollama /api/ps"
		},
		"window": func(o *Options) {
			o.Compact, o.CompactFrom, o.Window, o.WindowFrom = 49152, "window", 65536, "ollama /api/ps"
		},
		"off": func(o *Options) {},
		// The window unknown at the start: compaction is built with no
		// budget, which a switch sizes, so it is not off (review N3).
		"window unknown": func(o *Options) {
			o.CompactFrom, o.WindowOf = "window", windows(nil)
		},
	} {
		o := options(t, &echo.Adapter{})
		set(&o)
		s, err := New(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		h, _ := headerOf(t, o.Root, s)
		var c both
		if err := json.Unmarshal(h.Extra[headerContext], &c.recordedContext); err != nil {
			t.Fatalf("%s: %s = %s: %v", name, headerContext, h.Extra[headerContext], err)
		}
		if err := json.Unmarshal(h.Extra[headerContextFrom], &c.recordedContextFrom); err != nil {
			t.Fatalf("%s: %s = %s: %v", name, headerContextFrom, h.Extra[headerContextFrom], err)
		}
		got[name] = c
		s.Close()
	}
	if c := got["flag"]; c.Budget != 1000 || c.BudgetFrom != "flag" || c.Window != 65536 || c.WindowFrom != "ollama /api/ps" {
		t.Errorf("flag: %+v", c)
	}
	if c := got["window"]; c.Budget != 49152 || c.BudgetFrom != "window" {
		t.Errorf("window: %+v", c)
	}
	if c := got["off"]; c.Budget != 0 || c.BudgetFrom != "off" || c.Window != 0 {
		t.Errorf("off: %+v", c)
	}
	if c := got["window unknown"]; c.Budget != 0 || c.BudgetFrom != "window" || c.Window != 0 {
		t.Errorf("window unknown: %+v", c)
	}
	if got["flag"].recordedContext == got["window"].recordedContext {
		t.Error("two sessions that differ in their budget record the same context")
	}
}

// dax.context records the tool-output caps (#172) and the AGENTS.md
// budget in force, the defaults included, so two sessions that differ
// only in one of them compare as different configurations (#174).
func TestTheHeaderRecordsTheCapsAndTheAgentsMDBudget(t *testing.T) {
	ctx := context.Background()
	got := map[string]string{}
	for name, set := range map[string]func(*Options){
		"defaults": func(o *Options) {},
		"tool":     func(o *Options) { o.ToolMaxOutput = 4096 },
		"bash":     func(o *Options) { o.BashMaxOutput = 4096 },
		"agentsmd": func(o *Options) { o.AgentsMDBudget = 4096 },
		"project":  func(o *Options) { o.AgentsMDProjectBudget = 2048 },
	} {
		o := options(t, &echo.Adapter{})
		o.AgentsMD = true
		set(&o)
		s, err := New(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		h, _ := headerOf(t, o.Root, s)
		got[name] = string(h.Extra[headerContext])
		s.Close()
	}
	var c map[string]any
	if err := json.Unmarshal([]byte(got["defaults"]), &c); err != nil {
		t.Fatal(err)
	}
	if c["bash_max_output"] != float64(config.DefaultBashMaxOutput) || c["mcp_max_output"] != float64(config.DefaultToolMaxOutput) || c["agents_md_budget"] != float64(config.DefaultAgentsMDBudget) {
		t.Errorf("the defaults' dax.context = %s, want the caps and the budget in force", got["defaults"])
	}
	for _, name := range []string{"tool", "bash", "agentsmd", "project"} {
		if got[name] == got["defaults"] {
			t.Errorf("%s: dax.context %s is the defaults'", name, got[name])
		}
	}
}
