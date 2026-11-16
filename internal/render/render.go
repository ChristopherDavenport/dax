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
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ChristopherDavenport/agentconsole/view"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/tool"
)

const maxToolOutputShown = 800

// Printer writes a run to W. It is an agentturn subscriber; because
// every event is a barrier it runs on the loop's goroutine and needs no
// locking of its own.
type Printer struct {
	W io.Writer
	// Think shows reasoning deltas in a [thinking] block.
	Think bool
	// Compact says the front offers /compact, which the explanation of
	// a quota refusal then names (Explain).
	Compact bool

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
	case *agentturn.ModelBlocked:
		// A guard refused the model call: no response ends, and the
		// tokens the provider may have billed for it count.
		p.close()
		if u := e.Usage; u != nil {
			p.in += u.InputTokens
			p.out += u.OutputTokens
		}
		fmt.Fprintf(p.w(), "[model call refused: %v]\n", e.Err)
	case *agentturn.QuarantineAdded:
		// Made between runs, so no run's end says it.
		p.close()
		fmt.Fprintf(p.w(), "[quarantined %d item(s): left out of every request from now on]\n", len(e.Items))
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
		switch {
		case e.Reason == agentturn.ReasonStopped && e.Cause == agentturn.StopContentFilter:
			// The provider refused: what it said, and why when it
			// says (Anthropic's category), which no delta printed.
			fmt.Fprintln(p.w(), Refused(e))
			reason = "refused"
		case e.Reason == agentturn.ReasonStopped && e.Cause == agentturn.StopMaxOutputTokens:
			fmt.Fprintln(p.w(), "[the answer was cut at the model's output limit (max_output_tokens)]")
			reason = "cut"
		case e.Cause != "":
			reason += ": " + string(e.Cause)
		}
		fmt.Fprintf(p.w(), "[%s · %d model call(s) · %d tool call(s) · %d in / %d out tokens]\n", reason, p.turns, p.calls, p.in, p.out)
		if e.Err != nil && e.Reason != agentturn.ReasonAborted {
			if s, ok := Explain(e.Err, p.Compact); ok {
				fmt.Fprintf(p.w(), "[%s]\n", s)
			} else {
				fmt.Fprintf(p.w(), "[error: %v]\n", e.Err)
			}
		}
	}
	return nil
}

// Refused is the line that says what a run the provider's content
// filter stopped was refused with: "[refused: <category>] <text>",
// the category when the provider's details give one (Anthropic's), the
// text the refusal parts held, as every front shows it.
func Refused(e *agentturn.RunEnd) string {
	line := "[refused" + category(e.Incomplete) + "]"
	if e.Refusal != "" {
		line += " " + e.Refusal
	}
	return line
}

// category is ": " and a refusal's category, as the provider's
// incomplete details give it (Anthropic's anthropic.refusal_category),
// or "" when they give none.
func category(d *openresponses.IncompleteDetails) string {
	if d == nil {
		return ""
	}
	if c, ok := d.Extra["anthropic.refusal_category"].(string); ok && c != "" {
		return ": " + c
	}
	return ""
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
// taken out (view.Clean). Tool output is a file's contents and a
// command's output, and model text is whatever the model was shown;
// either can carry an escape sequence that redraws what the user has
// already read, such as the line that said what an approved call was.
// Each write is a whole string, so nothing is held back between them.
func (p *Printer) w() io.Writer { return cleanString{p.W} }

// cleanString writes each write cleaned on its own.
type cleanString struct{ w io.Writer }

func (c cleanString) Write(b []byte) (int, error) {
	if _, err := io.WriteString(c.w, view.Clean(string(b))); err != nil {
		return 0, err
	}
	return len(b), nil
}

// CleanWriter is w with view.Clean applied to what is written to it,
// for a stream that arrives in chunks, a child's standard error or a
// server's diagnostics: a character cut between two writes is held
// back until the write that completes it, where cleaning each chunk on
// its own would show it as two U+FFFD. A held start the next write does
// not continue is written as one U+FFFD before that write, so it never
// joins what follows. A character the stream ends inside is dropped:
// none of the streams dax cleans says when it ends, so give each
// process its own writer. It may be written from several goroutines.
func CleanWriter(w io.Writer) io.Writer { return &cleanStream{w: w} }

type cleanStream struct {
	mu   sync.Mutex
	w    io.Writer
	held []byte // the start of a character the last write cut
}

func (c *cleanStream) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var lead string
	buf := append(c.held, b...)
	if len(c.held) > 0 && len(b) > 0 && utf8.RuneStart(b[0]) {
		// The held start can never be completed: it is one cut
		// character.
		lead, buf = string(utf8.RuneError), b
	}
	cut := len(buf) - partialTail(buf)
	c.held = append([]byte(nil), buf[cut:]...)
	if lead == "" && cut == 0 {
		return len(b), nil
	}
	if _, err := io.WriteString(c.w, lead+view.Clean(string(buf[:cut]))); err != nil {
		return 0, err
	}
	return len(b), nil
}

// partialTail is how many bytes at the end of b begin a UTF-8 character
// that b does not finish: 0 when b ends on a whole one, or on bytes no
// further byte could make one, which view.Clean turns into U+FFFD
// either way.
func partialTail(b []byte) int {
	for i := len(b) - 1; i >= 0 && i > len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if utf8.FullRune(b[i:]) {
				return 0
			}
			return len(b) - i
		}
	}
	return 0
}
