package box

// This file is the redaction of a unit's secrets from what its server
// writes: the box host replaces each value its container holds before
// any of the server's output is buffered for the session, so a server
// that returns its own secret (an env tool, an error that quotes its
// token) hands the session, the model and the record a placeholder.
// It is a backstop, not a guarantee: a value the server encodes
// otherwise (base64, reversed, split across messages) is not found.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
)

// redactLineMax is the longest line the redaction holds whole: a line
// longer than this without its newline is passed on in pieces. A var
// for the tests.
var redactLineMax = 1 << 20

// Redactor replaces secret values, each in every spelling a JSON
// encoder writes it in, with "[REDACTED NAME]". The ordering is
// agentredact's Secrets' (the private sibling's strings.go), which this
// is modelled on: longest first, equal lengths in byte order, each
// replaced in what the longer ones left, duplicates once, empty values
// skipped, so the output does not depend on the order secrets are
// given in and a value holding another is not left half replaced.
type Redactor struct {
	spellings []spelling
	longest   int
}

type spelling struct {
	text []byte
	name string
}

// NewRedactor is a Redactor of values, by the secret's name. A value
// two names hold is replaced as the name that sorts first.
func NewRedactor(values map[string]string) *Redactor {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	seen := map[string]bool{}
	r := &Redactor{}
	for _, name := range names {
		v := values[name]
		if v == "" {
			continue
		}
		for _, s := range spellingsOf(v) {
			if s == "" || seen[s] {
				continue
			}
			seen[s] = true
			r.spellings = append(r.spellings, spelling{text: []byte(s), name: name})
			r.longest = max(r.longest, len(s))
		}
	}
	sort.Slice(r.spellings, func(i, j int) bool {
		a, b := r.spellings[i].text, r.spellings[j].text
		if len(a) != len(b) {
			return len(a) > len(b)
		}
		return bytes.Compare(a, b) < 0
	})
	return r
}

// Empty reports whether r has nothing to replace.
func (r *Redactor) Empty() bool { return r == nil || len(r.spellings) == 0 }

// spellingsOf is v as written and as JSON encoders write it inside a
// string: Go's with and without HTML escaping, each also with '/' as
// '\/', and the ASCII-only form Python's json.dumps writes (every
// non-ASCII rune as \uXXXX, a surrogate pair above U+FFFF), also with
// '/' as '\/' as PHP's json_encode writes it, with JavaScript's
// JSON.stringify's beside them (short escapes, other control
// characters as \u00XX, the rest as written).
func spellingsOf(v string) []string {
	goJSON := func(html bool) string {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(html)
		if err := enc.Encode(v); err != nil {
			return ""
		}
		s := strings.TrimSuffix(b.String(), "\n")
		return strings.TrimSuffix(strings.TrimPrefix(s, `"`), `"`)
	}
	plain, html := goJSON(false), goJSON(true)
	return []string{
		v,
		plain, html,
		strings.ReplaceAll(plain, "/", `\/`), strings.ReplaceAll(html, "/", `\/`),
		scriptJSON(v, true), strings.ReplaceAll(scriptJSON(v, true), "/", `\/`), scriptJSON(v, false),
	}
}

// scriptJSON is v as Python's json.dumps (ascii, every non-ASCII rune
// as \uXXXX) or JavaScript's JSON.stringify (not ascii) write it inside
// a string: \" \\ \b \f \n \r \t, other control characters as \u00XX,
// lower-case hex as both write it.
func scriptJSON(v string, ascii bool) string {
	var b strings.Builder
	for _, r := range v {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			switch {
			case r < 0x20:
				fmt.Fprintf(&b, `\u%04x`, r)
			case r < utf8.RuneSelf || !ascii:
				b.WriteRune(r)
			case r > 0xFFFF:
				r -= 0x10000
				fmt.Fprintf(&b, `\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
			default:
				fmt.Fprintf(&b, `\u%04x`, r)
			}
		}
	}
	return b.String()
}

// Redact is b with every spelling replaced, and how often each secret
// was, by name.
func (r *Redactor) Redact(b []byte) ([]byte, map[string]int) {
	if r.Empty() {
		return b, nil
	}
	var found map[string]int
	for _, s := range r.spellings {
		n := bytes.Count(b, s.text)
		if n == 0 {
			continue
		}
		if found == nil {
			found = map[string]int{}
		}
		found[s.name] += n
		b = bytes.ReplaceAll(b, s.text, []byte("[REDACTED "+s.name+"]"))
	}
	return b, found
}

// Reader is src as r redacts it: a line at a time, newline included, or,
// for a line longer than redactLineMax, in pieces that hold back
// whatever might begin a spelling the next piece ends. found is told of
// each replacement made.
func (r *Redactor) Reader(src io.Reader, found func(name string, n int)) io.Reader {
	if r.Empty() {
		return src
	}
	return &redactReader{r: r, src: src, found: found}
}

type redactReader struct {
	r     *Redactor
	src   io.Reader
	found func(string, int)

	in  []byte // read, not yet redacted
	out []byte // redacted, not yet read
	err error
}

func (rr *redactReader) Read(p []byte) (int, error) {
	for len(rr.out) == 0 {
		if rr.err != nil {
			if len(rr.in) > 0 {
				rr.emit(rr.in)
				rr.in = nil
				continue
			}
			return 0, rr.err
		}
		if i := bytes.IndexByte(rr.in, '\n'); i >= 0 {
			rr.emit(rr.in[:i+1])
			rr.in = rr.in[i+1:]
			continue
		}
		if len(rr.in) >= redactLineMax {
			// A cut at 0 is a spelling that may begin at the very start:
			// read on until it is whole or the line ends.
			if cut := rr.r.cut(rr.in); cut > 0 {
				rr.emit(rr.in[:cut])
				rr.in = rr.in[cut:]
				continue
			}
		}
		buf := make([]byte, 32<<10)
		n, err := rr.src.Read(buf)
		rr.in = append(rr.in, buf[:n]...)
		rr.err = err
	}
	n := copy(p, rr.out)
	rr.out = rr.out[n:]
	return n, nil
}

func (rr *redactReader) emit(b []byte) {
	red, found := rr.r.Redact(bytes.Clone(b))
	rr.out = append(rr.out, red...)
	if rr.found != nil {
		names := make([]string, 0, len(found))
		for name := range found {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			rr.found(name, found[name])
		}
	}
}

// cut is where to split b, a piece of a long line, so that no spelling
// is cut in two: the last longest-1 bytes are held back, and further
// still if a spelling starts before them and reaches into them.
func (r *Redactor) cut(b []byte) int {
	cut := len(b) - (r.longest - 1)
	if cut <= 0 {
		return len(b)
	}
	for e := max(0, cut-(r.longest-1)); e < cut; e++ {
		for _, s := range r.spellings {
			end := e + len(s.text)
			if end <= cut {
				continue
			}
			if bytes.HasPrefix(s.text, b[e:min(end, len(b))]) {
				return e
			}
		}
	}
	return cut
}

// unitSecretValues is the values of the secrets spec names as its
// container ctr holds them now, read with docker inspect, so what is
// redacted is what the container was given, whatever the box host's
// references say today. They are never logged or returned in an error.
func (c *Client) unitSecretValues(ctx context.Context, spec Spec, ctr string) (map[string]string, error) {
	if len(spec.Secrets) == 0 {
		return nil, nil
	}
	out, err := c.Runner.Docker(ctx, "inspect", "-f", "{{json .Config.Env}}", ctr)
	if err != nil {
		return nil, fmt.Errorf("unit %s: its secrets cannot be read to redact its output: %w", spec.Unit, err)
	}
	var env []string
	if err := json.Unmarshal(bytes.TrimSpace(out), &env); err != nil {
		return nil, fmt.Errorf("unit %s: its secrets cannot be read to redact its output: docker inspect gave no environment", spec.Unit)
	}
	values := map[string]string{}
	for _, kv := range env {
		name, v, ok := strings.Cut(kv, "=")
		if _, named := spec.Secrets[name]; ok && named {
			values[name] = v
		}
	}
	return values, nil
}

// StartUnitProc is StartProc of command in spec's container ctr, with
// what its server writes, standard output and error, redacted of the
// secrets the container holds; found is told of each secret replaced,
// by name. A container whose secrets cannot be read is not started.
func (c *Client) StartUnitProc(ctx context.Context, spec Spec, ctr string, command []string, stderr bool, found func(secret string, n int)) (Proc, error) {
	values, err := c.unitSecretValues(ctx, spec, ctr)
	if err != nil {
		return nil, err
	}
	p, err := c.StartProc(ctx, ctr, command, stderr)
	if err != nil {
		return nil, err
	}
	r := NewRedactor(values)
	if r.Empty() {
		return p, nil
	}
	rp := &redactedProc{Proc: p, out: r.Reader(p.Stdout(), found)}
	if e := p.Stderr(); e != nil {
		rp.err = r.Reader(e, found)
	}
	return rp, nil
}

// redactedProc is a Proc whose output is read through a Redactor.
type redactedProc struct {
	Proc
	out, err io.Reader
}

func (p *redactedProc) Stdout() io.Reader { return p.out }

func (p *redactedProc) Stderr() io.Reader {
	if p.err == nil {
		return nil
	}
	return p.err
}
