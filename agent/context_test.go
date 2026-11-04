package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ChristopherDavenport/openresponses/echo"
)

// The header says what context strategy the session ran: its budget,
// where that came from, and the window dax believed, so two sessions
// that differ only in -compact compare as different (#174).
func TestTheHeaderRecordsTheContextSettings(t *testing.T) {
	ctx := context.Background()
	got := map[string]recordedContext{}
	for name, set := range map[string]func(*Options){
		"flag": func(o *Options) {
			o.Compact, o.CompactFrom, o.Window, o.WindowFrom = 1000, "flag", 65536, "ollama /api/ps"
		},
		"window": func(o *Options) {
			o.Compact, o.CompactFrom, o.Window, o.WindowFrom = 49152, "window", 65536, "ollama /api/ps"
		},
		"off": func(o *Options) {},
	} {
		o := options(t, &echo.Adapter{})
		set(&o)
		s, err := New(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		h, _ := headerOf(t, o.Root, s)
		var c recordedContext
		if err := json.Unmarshal(h.Extra[headerContext], &c); err != nil {
			t.Fatalf("%s: %s = %s: %v", name, headerContext, h.Extra[headerContext], err)
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
	if got["flag"] == got["window"] {
		t.Error("two sessions that differ in their budget record the same context")
	}
}
