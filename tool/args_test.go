package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
)

// A key that names a field in another case is read by the tool's
// decoder (any case, the last such key wins) and not by a claim that
// reads the exact key, so the policy would be asked about one path and
// the tool act on another: {"path":"notes.txt","Path":".env"} was an
// auto-allowed read of .env. Both sides refuse such arguments, each on
// its own: the claim fails, which the policy blocks, and the tool fails
// when it runs, acting on nothing. (The tool refuses any of its fields
// in another case; a claim, the fields it reads.)
func TestAKeyInAnotherCaseIsRefusedByTheClaimAndTheTool(t *testing.T) {
	for _, tc := range []struct {
		name string
		tool func(*Files) agenttool.Tool
		args string
	}{
		{"read Path", func(f *Files) agenttool.Tool { return Read(f) }, `{"path":"notes.txt","Path":".env"}`},
		{"read PATH alone", func(f *Files) agenttool.Tool { return Read(f) }, `{"PATH":".env"}`},
		{"read a stamp in another case", func(f *Files) agenttool.Tool { return Read(f) }, `{"path":"notes.txt","DAX_STAMP":"x"}`},
		{"read a stamp with the long s", func(f *Files) agenttool.Tool { return Read(f) }, `{"path":"notes.txt","dax_ſtamp":"x"}`},
		{"write Path", func(f *Files) agenttool.Tool { return Write(f) }, `{"path":"notes.txt","content":"pwn","Path":"victim.txt"}`},
		{"edit Path", func(f *Files) agenttool.Tool { return Edit(f) }, `{"path":"notes.txt","old_string":"notes","new_string":"pwn","Path":"victim.txt"}`},
		{"glob Path", func(f *Files) agenttool.Tool { return Glob(f) }, `{"pattern":"*","path":".","Path":"secret"}`},
		{"grep Path", func(f *Files) agenttool.Tool { return Grep(f) }, `{"pattern":"SECRET","path":"notes.txt","Path":".env"}`},
		{"ls Path", func(f *Files) agenttool.Tool { return LS(f) }, `{"path":".","Path":"secret"}`},
		{"bash Command", func(f *Files) agenttool.Tool { return Bash(f) }, `{"command":"pwd","Command":"touch victim.txt"}`},
		{"bash a stamp in another case", func(f *Files) agenttool.Tool { return Bash(f) }, `{"command":"touch victim.txt","Dax_Stamp":"x"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("notes\n"), 0o644)
			os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=1\n"), 0o644)
			f := newWS(t, dir)
			tl := tc.tool(f)
			if _, _, err := agenttool.FactsOf(ctx, tl, json.RawMessage(tc.args)); err == nil || !strings.Contains(err.Error(), "another case") {
				t.Errorf("the claim = %v, want refused", err)
			}
			out, err := call(ctx, tl, tc.args)
			if err == nil || !strings.Contains(err.Error(), "another case") {
				t.Errorf("the tool ran: %q, %v", out, err)
			}
			if _, err := os.Stat(filepath.Join(dir, "victim.txt")); err == nil {
				t.Error("victim.txt was written")
			}
			if b, _ := os.ReadFile(filepath.Join(dir, "notes.txt")); string(b) != "notes\n" {
				t.Errorf("notes.txt = %q", b)
			}
		})
	}
	// The exported splitters refuse them too.
	f := newWS(t, t.TempDir())
	if _, err := PathSubjects(f, "path", "")(json.RawMessage(`{"path":"a","Path":"b"}`)); err == nil {
		t.Error("PathSubjects read a key in another case")
	}
	if _, err := BashSubjects(f, 0)(json.RawMessage(`{"command":"pwd","COMMAND":"rm x"}`)); err == nil {
		t.Error("BashSubjects read a key in another case")
	}
	if _, _, err := StampArgs(context.Background(), &Analyzer{Files: f}, json.RawMessage(`{"command":"pwd","Command":"rm x"}`)); err == nil {
		t.Error("StampArgs read a key in another case")
	}
}
