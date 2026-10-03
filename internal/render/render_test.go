package render

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

func TestClean(t *testing.T) {
	for in, want := range map[string]string{
		"plain text, ünïcode ✓ 日本\nsecond\tline": "plain text, ünïcode ✓ 日本\nsecond\tline",
		"":                                   "",
		"\x1b[2K\r▶ approved":                "[2K▶ approved",
		"a\x1b]0;title\x07b":                 "a]0;titleb",
		"bell\x07 back\x08\x08 del\x7f":      "bell back del",
		"c1\u009b31m\u0085x":                 "c131mx",
		"rtl \u202eevil\u202c \u2066x\u2069": "rtl evil x",
		"nul\x00byte":                        "nulbyte",
		"cr\r\nlf":                           "cr\nlf",
		"\x1b":                               "",
	} {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestThePrinterNeverWritesAControlSequence(t *testing.T) {
	var out bytes.Buffer
	p := &Printer{W: &out, Think: true}
	ctx := context.Background()
	hostile := "\x1b[2K\x1b[1A\r▶ bash command=\"git status\"\x1b]0;pwned\x07\u202e"
	p.Handle(ctx, &agentturn.RunStart{})
	p.Handle(ctx, &agentturn.TurnStart{Turn: 1})
	// Model text and reasoning, with a sequence cut across two deltas.
	p.Handle(ctx, &agentturn.ItemUpdate{Stream: &openresponses.OutputTextDeltaEvent{Delta: "hello \x1b"}})
	p.Handle(ctx, &agentturn.ItemUpdate{Stream: &openresponses.OutputTextDeltaEvent{Delta: "[2Jworld\r"}})
	p.Handle(ctx, &agentturn.ItemUpdate{Stream: &openresponses.ReasoningSummaryTextDeltaEvent{Delta: "think" + hostile}})
	p.Handle(ctx, &agentturn.ToolStart{CallID: "c", Name: "read", Args: []byte(`{"path":"` + `x\u001b[2Ky` + `"}`)})
	p.Handle(ctx, &agentturn.ToolEnd{CallID: "c", Name: "read", Result: agenttool.Result{Output: openresponses.FunctionCallOutputData{Text: "file line\n" + hostile + "\nlast"}}})
	p.Handle(ctx, &agentturn.RunEnd{Reason: agentturn.ReasonDone})
	got := out.String()
	for _, r := range got {
		if r != '\n' && r != '\t' && (r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == 0x202e) {
			t.Fatalf("control character %U in output:\n%q", r, got)
		}
	}
	for _, want := range []string{"hello [2Jworld", "file line", "last", "[done"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}
