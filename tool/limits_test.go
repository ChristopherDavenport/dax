package tool

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// #10 of the review: a 768 MB sparse file allocated 3 GB in one read.
func TestReadOfAHugeFileIsBounded(t *testing.T) {
	dir := t.TempDir()
	huge := filepath.Join(dir, "huge")
	f, err := os.Create(huge)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("first line\nsecond line\n")
	if err := f.Truncate(768 << 20); err != nil { // sparse: zeros, one enormous last line
		t.Fatal(err)
	}
	f.Close()
	ws := newWS(t, dir)

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	out, err := call(context.Background(), Read(ws), `{"path":"huge"}`)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 64<<20 {
		t.Errorf("read of a 768 MiB file allocated %d MiB", alloc>>20)
	}
	for _, want := range []string{"     1\tfirst line", "     2\tsecond line", "[line truncated]", "stopped after the first 2097152 of 805306368 bytes"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %.300q", want, out)
		}
	}
	if len(out) > 300<<10 {
		t.Errorf("output is %d bytes", len(out))
	}

	// An offset past the part read says so rather than scanning on.
	out, err = call(context.Background(), Read(ws), `{"path":"huge","offset":5}`)
	if err != nil || !strings.Contains(out, "beyond the first 2097152 bytes") {
		t.Errorf("deep offset: %q, %v", out, err)
	}

	// The cap is configurable.
	small := Read(ws, WithMaxRead(8))
	out, _ = call(context.Background(), small, `{"path":"huge"}`)
	if !strings.Contains(out, "stopped after the first 8 of") || strings.Contains(out, "second") {
		t.Errorf("WithMaxRead(8): %q", out)
	}
}

func TestReadEdgeCases(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "crlf"), []byte("a\r\nb\r\nlast no newline"), 0o644)
	os.WriteFile(filepath.Join(dir, "empty"), nil, 0o644)
	os.Mkdir(filepath.Join(dir, "d"), 0o755)
	ws := newWS(t, dir)
	out, _ := call(context.Background(), Read(ws), `{"path":"crlf"}`)
	if out != "     1\ta\n     2\tb\n     3\tlast no newline\n" {
		t.Errorf("crlf: %q", out)
	}
	if out, _ := call(context.Background(), Read(ws), `{"path":"empty"}`); !strings.Contains(out, "file has 0 lines") {
		t.Errorf("empty: %q", out)
	}
	if _, err := call(context.Background(), Read(ws), `{"path":"d"}`); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("directory: %v", err)
	}
	if out, _ := call(context.Background(), Read(ws), `{"path":"crlf","offset":9}`); !strings.Contains(out, "file has 3 lines; offset 9 is past the end") {
		t.Errorf("past the end: %q", out)
	}
}

func TestEditRefusesAFileOverTheCap(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "big"), []byte(strings.Repeat("x", 100)), 0o644)
	ws := newWS(t, dir)
	_, err := call(context.Background(), Edit(ws, WithMaxRead(50)), `{"path":"big","old_string":"x","new_string":"y"}`)
	if err == nil || !strings.Contains(err.Error(), "over the 50-byte limit") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "big")); string(b) != strings.Repeat("x", 100) {
		t.Error("the file changed")
	}
}

// #12 of the review: a symlink to a FIFO inside the workspace blocked
// grep forever.
func TestSearchDoesNotOpenAFIFO(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o644); err != nil {
		t.Skip("no mkfifo:", err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Symlink("fifo", filepath.Join(dir, "link")))
	os.WriteFile(filepath.Join(dir, "real.txt"), []byte("needle\n"), 0o644)
	ws := newWS(t, dir)
	for name, args := range map[string]string{
		"whole tree": `{"pattern":"needle"}`,
		"the link":   `{"pattern":"needle","path":"link"}`,
		"the fifo":   `{"pattern":"needle","path":"fifo"}`,
	} {
		done := make(chan string, 1)
		go func() {
			out, err := call(context.Background(), Grep(ws), args)
			if err != nil {
				out = "error: " + err.Error()
			}
			done <- out
		}()
		select {
		case out := <-done:
			if name == "whole tree" && !strings.Contains(out, "real.txt:1:needle") {
				t.Errorf("%s: %q", name, out)
			}
			if name != "whole tree" && strings.Contains(out, "needle") {
				t.Errorf("%s: %q", name, out)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("grep over %s blocked on a FIFO", name)
		}
	}
	out, _ := call(context.Background(), Glob(ws), `{"pattern":"**"}`)
	if strings.Contains(out, "fifo") || strings.Contains(out, "link") || !strings.Contains(out, "real.txt") {
		t.Errorf("glob: %q", out)
	}
	if _, err := call(context.Background(), Read(ws), `{"path":"link"}`); err == nil {
		t.Error("read of a FIFO should be refused, not block")
	}
}

// #11 of the review: **/a/**/a/... against a deep path took minutes.
func TestGlobMatchingIsNotExponential(t *testing.T) {
	name := strings.Repeat("a/", 40) + "b"
	for _, k := range []int{7, 10, 20} {
		pat := strings.Repeat("**/a/", k) + "z"
		start := time.Now()
		if matchGlob(pat, name) {
			t.Errorf("k=%d matched", k)
		}
		if d := time.Since(start); d > 500*time.Millisecond {
			t.Errorf("k=%d took %s", k, d)
		}
	}
	if !matchGlob(strings.Repeat("**/a/", 10)+"b", name) {
		t.Error("a matching pattern did not match")
	}
}

func TestSearchStopsWhenCancelled(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 200; i++ {
		files[filepath.Join("d", string(rune('a'+i%26)), strings.Repeat("x", i%7+1)+string(rune('a'+i/26))+".txt")] = "needle\n"
	}
	ws := newWS(t, tree(t, files))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := call(ctx, Grep(ws), `{"pattern":"needle"}`); err == nil {
		t.Error("a cancelled grep should stop")
	}
	if _, err := call(ctx, Glob(ws), `{"pattern":"**/*.txt"}`); err == nil {
		t.Error("a cancelled glob should stop")
	}
}
