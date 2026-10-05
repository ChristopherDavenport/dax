// Package render is the print front: an agentturn subscriber that
// streams a run to a writer as it happens.
package render

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/internal/tool"
)

const maxToolOutputShown = 800

// Printer writes a run to W. It is an agentturn subscriber; because
// every event is a barrier it runs on the loop's goroutine and needs no
// locking of its own.
type Printer struct {
	W io.Writer
	// Think shows reasoning deltas in a [thinking] block.
	Think bool

	kind    string
	open    bool
	in, out int
	calls   int
	turns   int
	// started remembers each call of the current turn by ID, so a batch
	// of more than one call has its outputs labelled: tool_end arrives
	// in completion order, not in the order tool_start announced them.
	started map[string]string
}

// Handle is the subscriber function.
func (p *Printer) Handle(_ context.Context, ev agentturn.Event) error {
	switch e := ev.(type) {
	case *agentturn.RunStart:
		p.in, p.out, p.calls, p.turns = 0, 0, 0, 0
		// A resumed run's batch runs as turn 0, before any TurnStart.
		p.started = map[string]string{}
	case *agentturn.TurnStart:
		p.started = map[string]string{}
		// Inputs are the items this turn's response answers. Tool
		// outputs among them were printed as they ended; a user message
		// here is a steer or a follow-up landing, which the user queued
		// some turns ago and has not seen take effect.
		if e.Turn > 1 {
			for _, it := range e.Inputs {
				if m, ok := it.(*openresponses.Message); ok && m.Role == openresponses.RoleUser {
					fmt.Fprintf(p.w(), "  ↳ picked up: %s\n", firstLine(m.Text()))
				}
			}
		}
	case *agentturn.ItemUpdate:
		switch s := e.Stream.(type) {
		case *openresponses.OutputTextDeltaEvent:
			p.write("", s.Delta)
		case *openresponses.ReasoningSummaryTextDeltaEvent:
			if p.Think {
				p.write("thinking", s.Delta)
			}
		case *openresponses.ReasoningDeltaEvent:
			if p.Think {
				p.write("thinking", s.Delta)
			}
		}
	case *agentturn.ModelRetry:
		p.close()
		fmt.Fprintf(p.w(), "[model call failed (attempt %d): %v; retrying in %s]\n", e.Attempt, e.Err, e.Delay.Round(time.Millisecond))
	case *agentturn.ResponseEnd:
		p.close()
		p.turns++
		if u := e.Response.Usage; u != nil {
			p.in += u.InputTokens
			p.out += u.OutputTokens
		}
	case *agentturn.ToolStart:
		p.close()
		p.calls++
		label := e.Name + " " + compactArgs(e.Args)
		p.started[e.CallID] = label
		fmt.Fprintf(p.w(), "▶ %s\n", label)
	case *agentturn.ToolUpdate:
		// A child agent reports each assistant message it produces.
		if txt := tool.Text(e.Partial); txt != "" {
			fmt.Fprintf(p.w(), "  ↳ %s: %s\n", e.Name, firstLine(txt))
		}
	case *agentturn.ToolEnd:
		if e.Deferred {
			fmt.Fprintf(p.w(), "  ⏸ %s deferred\n", e.Name)
			return nil
		}
		shown := tool.Text(e.Result)
		if len(shown) > maxToolOutputShown {
			shown = shown[:maxToolOutputShown] + "…"
		}
		mark := "│"
		if e.Err != nil {
			mark = "✗"
		}
		if len(p.started) > 1 {
			fmt.Fprintf(p.w(), "  %s [%s]\n", mark, p.started[e.CallID])
		}
		for line := range strings.SplitSeq(strings.TrimRight(shown, "\n"), "\n") {
			fmt.Fprintf(p.w(), "  %s %s\n", mark, line)
		}
	case *agentturn.RunEnd:
		p.close()
		// Cause says what stopped a run the loop ended itself, which
		// "stopped" alone does not.
		reason := string(e.Reason)
		if e.Cause != "" {
			reason += ": " + string(e.Cause)
		}
		fmt.Fprintf(p.w(), "[%s · %d model call(s) · %d tool call(s) · %d in / %d out tokens]\n", reason, p.turns, p.calls, p.in, p.out)
		if e.Err != nil && e.Reason != agentturn.ReasonAborted {
			fmt.Fprintf(p.w(), "[error: %v]\n", e.Err)
		}
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + "…"
	}
	if len(s) > 100 {
		s = s[:100] + "…"
	}
	return s
}

func compactArgs(raw json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return string(raw)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(m))
	for _, k := range keys {
		s := fmt.Sprint(m[k])
		if len(s) > 60 {
			s = s[:60] + "…"
		}
		parts = append(parts, fmt.Sprintf("%s=%q", k, s))
	}
	return strings.Join(parts, " ")
}

// write streams a delta, opening a labelled block when the kind changes.
func (p *Printer) write(kind, delta string) {
	if !p.open || p.kind != kind {
		if p.open {
			fmt.Fprintln(p.w())
		}
		if kind != "" {
			fmt.Fprintf(p.w(), "[%s]\n", kind)
		}
		p.kind, p.open = kind, true
	}
	io.WriteString(p.w(), delta)
}

func (p *Printer) close() {
	if p.open {
		fmt.Fprintln(p.w())
		p.open = false
	}
}

// w is the writer everything goes through: W, with control sequences
// taken out. Tool output is a file's contents and a command's output,
// and model text is whatever the model was shown; either can carry an
// escape sequence that redraws what the user has already read, such
// as the line that said what an approved call was.
func (p *Printer) w() io.Writer { return cleanWriter{p.W} }

// CleanWriter is w with Clean applied to everything written to it.
func CleanWriter(w io.Writer) io.Writer { return cleanWriter{w} }

type cleanWriter struct{ w io.Writer }

func (c cleanWriter) Write(b []byte) (int, error) {
	if _, err := io.WriteString(c.w, Clean(string(b))); err != nil {
		return 0, err
	}
	return len(b), nil
}

// Clean removes what would act on a terminal rather than show on it:
// the C0 controls other than newline and tab (ESC, carriage return,
// backspace, bell, ...), DEL, the C1 controls, and the bidirectional
// overrides that make text read as something else. An escape sequence
// loses its ESC and shows as the harmless text it would have been;
// removing the one byte, rather than parsing the sequence, is what
// keeps a sequence cut in two by streaming deltas from working.
func Clean(s string) string {
	clean := true
	for _, r := range s {
		if bad(r) {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if !bad(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func bad(r rune) bool {
	switch {
	case r == '\n' || r == '\t':
		return false
	case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
		return true
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069, r == 0x200e, r == 0x200f, r == 0x061c:
		return true
	}
	return false
}
