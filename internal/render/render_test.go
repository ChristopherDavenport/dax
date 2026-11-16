package render

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentconsole/view"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// dax cleans with agentconsole's view.Clean, which drops what dax's
// own cleaner dropped, and turns a byte that is not UTF-8 into U+FFFD.
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
		"raw \x9b31m":                        "raw \uFFFD31m",
	} {
		if got := view.Clean(in); got != want {
			t.Errorf("view.Clean(%q) = %q, want %q", in, got, want)
		}
	}
}

// A character a pipe delivers in two chunks reaches the terminal whole
// through CleanWriter, an escape before it in the same chunk or not
// (review L5); one the stream ends inside is U+FFFD at Close.
func TestCleanWriterJoinsACharacterSplitAcrossWrites(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks []string
		want   string
	}{
		{"two bytes", []string{"caf\xc3", "\xa9 ok"}, "café ok"},
		{"after an escape", []string{"\x1b[2Jcaf\xc3", "\xa9"}, "[2Jcafé"},
		{"four bytes over three writes", []string{"a\xf0\x9f", "\x98", "\x80b"}, "a😀b"},
		{"cut at the end", []string{"x\xe6\x97"}, "x\uFFFD"},
		{"invalid", []string{"x\xff", "y"}, "x\uFFFDy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			w := CleanWriter(&out)
			for _, c := range tc.chunks {
				if n, err := w.Write([]byte(c)); err != nil || n != len(c) {
					t.Fatalf("Write(%q) = %d, %v", c, n, err)
				}
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if out.String() != tc.want {
				t.Errorf("wrote %q, want %q", out.String(), tc.want)
			}
		})
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

// A model call a guard refused is said, its tokens counted, and a
// quarantine made between runs is said too: neither reaches a
// response's end.
func TestThePrinterSaysWhatAGuardDid(t *testing.T) {
	var b strings.Builder
	p := &Printer{W: &b}
	ctx := context.Background()
	for _, ev := range []agentturn.Event{
		&agentturn.RunStart{},
		&agentturn.ModelBlocked{Err: errors.New("flagged: injection"), Usage: &openresponses.Usage{InputTokens: 7, OutputTokens: 2}},
		&agentturn.RunEnd{Reason: agentturn.ReasonStopped, Cause: agentturn.StopGuard},
		&agentturn.QuarantineAdded{Items: openresponses.Items{openresponses.UserText("x"), openresponses.UserText("y")}},
	} {
		if err := p.Handle(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	out := b.String()
	for _, want := range []string{"[model call refused: flagged: injection]", "[stopped: guard · 0 model call(s) · 0 tool call(s) · 7 in / 2 out tokens]", "[quarantined 2 item(s): left out of every request from now on]"} {
		if !strings.Contains(out, want) {
			t.Errorf("printed:\n%s\nwant %q", out, want)
		}
	}
}

// A run the provider's content filter stopped prints the refusal and
// its category, which no delta printed, and ends refused; one whose
// answer the output limit cut says so and ends cut (#206).
func TestThePrinterSaysARefusalAndACut(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  *agentturn.RunEnd
		want []string
	}{
		{"refused", &agentturn.RunEnd{Reason: agentturn.ReasonStopped, Cause: agentturn.StopContentFilter, Refusal: "I can't help with that.",
			Incomplete: &openresponses.IncompleteDetails{Reason: openresponses.IncompleteReasonContentFilter, Extra: map[string]any{"anthropic.refusal_category": "cyber"}}},
			[]string{"[refused: cyber] I can't help with that.\n", "[refused · 0 model call(s)"}},
		{"refused with no category", &agentturn.RunEnd{Reason: agentturn.ReasonStopped, Cause: agentturn.StopContentFilter, Refusal: "No.",
			Incomplete: &openresponses.IncompleteDetails{Reason: openresponses.IncompleteReasonContentFilter}},
			[]string{"[refused] No.\n", "[refused · "}},
		{"cut", &agentturn.RunEnd{Reason: agentturn.ReasonStopped, Cause: agentturn.StopMaxOutputTokens,
			Incomplete: &openresponses.IncompleteDetails{Reason: openresponses.IncompleteReasonMaxOutputTokens}},
			[]string{"[the answer was cut at the model's output limit (max_output_tokens)]\n", "[cut · 0 model call(s)"}},
	} {
		var b strings.Builder
		p := &Printer{W: &b}
		for _, ev := range []agentturn.Event{&agentturn.RunStart{}, tc.end} {
			if err := p.Handle(context.Background(), ev); err != nil {
				t.Fatal(err)
			}
		}
		for _, w := range tc.want {
			if !strings.Contains(b.String(), w) {
				t.Errorf("%s: printed:\n%s\nwant %q", tc.name, b.String(), w)
			}
		}
	}
}
