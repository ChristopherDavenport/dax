package box

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"
)

// One value, every spelling a JSON encoder writes it in, written out
// by hand rather than by the code under test: a slash, a quote, a
// backslash, HTML's three, a two-byte rune, an astral one, a newline
// and a control character.
func TestRedactSpellings(t *testing.T) {
	const v = "p/q\"r\\s<t>&é😀\n\x01"
	r := NewRedactor(map[string]string{"T": v})
	for _, tc := range []struct{ name, spelling string }{
		{"as written", v},
		{"Go, no HTML escaping (and JavaScript)", `p/q\"r\\s<t>&é😀\n\u0001`},
		{"Go, HTML escaping", `p/q\"r\\s\u003ct\u003e\u0026é😀\n\u0001`},
		{"slashes escaped", `p\/q\"r\\s<t>&é😀\n\u0001`},
		{"slashes and HTML escaped", `p\/q\"r\\s\u003ct\u003e\u0026é😀\n\u0001`},
		{"Python, ensure_ascii", `p/q\"r\\s<t>&\u00e9\ud83d\ude00\n\u0001`},
		{"PHP, slashes and ASCII escaped", `p\/q\"r\\s<t>&\u00e9\ud83d\ude00\n\u0001`},
	} {
		got, found := r.Redact([]byte("x" + tc.spelling + "y"))
		if string(got) != "x[REDACTED T]y" || found["T"] != 1 {
			t.Errorf("%s: %q, found %v", tc.name, got, found)
		}
	}
	// A JSON line as each encoder writes it holds no part of the value.
	for _, html := range []bool{false, true} {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(html)
		if err := enc.Encode(map[string]string{"env": "T=" + v}); err != nil {
			t.Fatal(err)
		}
		got, _ := r.Redact(b.Bytes())
		if string(got) != `{"env":"T=[REDACTED T]"}`+"\n" {
			t.Errorf("html %v: %s", html, got)
		}
	}
}

func TestRedactValues(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values map[string]string
		in     string
		want   string
	}{
		{"a value holding another", map[string]string{"SHORT": "secret", "LONG": "secret-long"}, "secret-long and secret", "[REDACTED LONG] and [REDACTED SHORT]"},
		{"overlapping, one length: byte order", map[string]string{"X": "abcd", "Y": "bcde"}, "abcde", "[REDACTED X]e"},
		{"empty values skipped", map[string]string{"E": "", "F": "f00"}, "a f00 b", "a [REDACTED F] b"},
		{"one value, two names: the first name", map[string]string{"B": "same", "A": "same"}, "same", "[REDACTED A]"},
		{"nothing to find", map[string]string{"A": "absent"}, "present", "present"},
		{"quotes and backslashes", map[string]string{"Q": `a"b\c`}, `{"v":"a\"b\\c"}`, `{"v":"[REDACTED Q]"}`},
		{"a tab and a carriage return", map[string]string{"W": "a\tb\rc"}, `"a\tb\rc"`, `"[REDACTED W]"`},
	} {
		// Built many times, since a map's order is not fixed: the output
		// must not depend on it.
		for range 20 {
			got, _ := NewRedactor(tc.values).Redact([]byte(tc.in))
			if string(got) != tc.want {
				t.Fatalf("%s: %q, want %q", tc.name, got, tc.want)
			}
		}
	}
	if !NewRedactor(map[string]string{"E": ""}).Empty() || !NewRedactor(nil).Empty() {
		t.Error("a redactor of nothing is not empty")
	}
}

// A line longer than redactLineMax is passed on in pieces, and a value
// split across where a piece would end is still found, read a byte at
// a time or all at once.
func TestRedactReaderLongLines(t *testing.T) {
	old := redactLineMax
	redactLineMax = 64
	defer func() { redactLineMax = old }()
	const v = "0123456789-secret-value"
	r := NewRedactor(map[string]string{"S": v})
	var in strings.Builder
	var want strings.Builder
	for i := range 40 {
		// The value lands at every offset against the pieces' ends.
		pad := strings.Repeat("x", i)
		in.WriteString(pad + v + pad)
		want.WriteString(pad + "[REDACTED S]" + pad)
	}
	in.WriteString("\nnext " + v + "\n" + "no newline at the end " + v)
	want.WriteString("\nnext [REDACTED S]\nno newline at the end [REDACTED S]")
	for name, src := range map[string]func() io.Reader{
		"a byte at a time": func() io.Reader { return iotest.OneByteReader(strings.NewReader(in.String())) },
		"all at once":      func() io.Reader { return strings.NewReader(in.String()) },
	} {
		n := 0
		got, err := io.ReadAll(r.Reader(src(), func(name string, k int) {
			if name != "S" {
				t.Errorf("found %s", name)
			}
			n += k
		}))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want.String() {
			t.Errorf("%s:\n got %q\nwant %q", name, got, want.String())
		}
		if strings.Contains(string(got), v) || n != 42 {
			t.Errorf("%s: the value is left, or %d found, want 42", name, n)
		}
	}
}

// A unit's server that writes its own secret: the session reads the
// placeholder on standard output and error, never the value, and the
// box host is told each secret once.
func TestStartUnitProcRedacts(t *testing.T) {
	const v = `tok"en/1é`
	script := filepath.Join(t.TempDir(), "server")
	if err := os.WriteFile(script, []byte(`cat <<'EOF'
{"result":{"env":{"DAX_TEST_TOKEN":"tok\"en/1é"}}}
{"result":{"ascii":"tok\"en\/1\u00e9","raw":"tok"en/1é"}}
EOF
echo 'err tok"en/1é' >&2
`), 0o755); err != nil {
		t.Fatal(err)
	}
	env, _ := json.Marshal([]string{"PATH=/bin", "DAX_TEST_TOKEN=" + v, "UNNAMED=tok"})
	f := &fake{answers: map[string][]answer{"inspect": {{out: string(env)}}}}
	c := &Client{Runner: f, DockerPath: standInDocker(t)}
	spec := Spec{Unit: "env", Secrets: map[string]string{"DAX_TEST_TOKEN": "env:DAX_TEST_TOKEN"}}
	var mu sync.Mutex
	told := map[string]int{}
	procs := newHostProcs(func(ctx context.Context, unit string, stderr bool) (Proc, error) {
		return c.StartUnitProc(ctx, spec, "unit", []string{"sh", script}, stderr, func(secret string, n int) {
			mu.Lock()
			told[secret] += n
			mu.Unlock()
		})
	})
	defer procs.close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := procs.serve(ctx, methodStart, json.RawMessage(`{"args":["dax-unit","env"],"stderr":true}`))
	if err != nil {
		t.Fatal(err)
	}
	id := res.(map[string]any)["id"].(string)
	read := func(fd int) string {
		var all []byte
		for {
			params, _ := json.Marshal(map[string]any{"id": id, "fd": fd})
			r, err := procs.serve(ctx, methodRead, params)
			if err != nil {
				t.Fatal(err)
			}
			m := r.(map[string]any)
			if d, ok := m["data"].([]byte); ok {
				all = append(all, d...)
			}
			if m["eof"] == true {
				return string(all)
			}
		}
	}
	out, errOut := read(1), read(2)
	wantOut := `{"result":{"env":{"DAX_TEST_TOKEN":"[REDACTED DAX_TEST_TOKEN]"}}}` + "\n" +
		`{"result":{"ascii":"[REDACTED DAX_TEST_TOKEN]","raw":"[REDACTED DAX_TEST_TOKEN]"}}` + "\n"
	if out != wantOut {
		t.Errorf("stdout:\n got %q\nwant %q", out, wantOut)
	}
	if errOut != "err [REDACTED DAX_TEST_TOKEN]\n" {
		t.Errorf("stderr: %q", errOut)
	}
	if strings.Contains(out+errOut, v) || strings.Contains(out+errOut, `tok\"en`) {
		t.Errorf("the value reached the session: %q %q", out, errOut)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(told) != 1 || told["DAX_TEST_TOKEN"] != 4 {
		t.Errorf("told %v, want DAX_TEST_TOKEN 4 times", told)
	}
	// What is read is the container's environment, by the secrets' names.
	if got := strings.Join(f.calls[0], " "); got != "inspect -f {{json .Config.Env}} unit" {
		t.Errorf("docker %s", got)
	}
}

// A container whose secrets cannot be read is not started, and the
// error holds nothing docker printed on standard output.
func TestStartUnitProcUnreadable(t *testing.T) {
	f := &fake{answers: map[string][]answer{"inspect": {{out: "not json tok"}}}}
	c := &Client{Runner: f, DockerPath: standInDocker(t)}
	spec := Spec{Unit: "env", Secrets: map[string]string{"T": "env:T"}}
	_, err := c.StartUnitProc(context.Background(), spec, "unit", []string{"true"}, false, nil)
	if err == nil || strings.Contains(err.Error(), "tok") || !strings.Contains(err.Error(), "cannot be read") {
		t.Errorf("err = %v", err)
	}
	// A unit with no secrets reads nothing and is not wrapped.
	c = &Client{Runner: &fake{}, DockerPath: standInDocker(t)}
	p, err := c.StartUnitProc(context.Background(), Spec{Unit: "plain"}, "unit", []string{"true"}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.Close()
	if _, wrapped := p.(*redactedProc); wrapped {
		t.Error("a unit without secrets was wrapped")
	}
}
