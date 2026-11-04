package agent

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/extension"
)

// keptRef is the opaque id a cut result names its kept whole by; the
// blob's hash is never in the text (review R2-H1).
var keptRef = regexp.MustCompile(`out_[0-9a-f]{16}`)

// big is an extension whose tool returns a megabyte, as an MCP server
// may.
func big() extension.Extension {
	return extension.Extension{Name: "big", Tools: func(extension.ToolEnv) []agenttool.Tool {
		return []agenttool.Tool{agenttool.New("dump", "dump a lot", func(context.Context, struct{}) (string, error) {
			return strings.Repeat("x", 1<<20) + "\nLAST", nil
		})}
	}}
}

// A result over its cap is sent as its head and its tail, so the end of
// a log, where the error is, arrives; the whole is kept with the session
// and read back by read_output (#172).
func TestALongResultIsCutAndKept(t *testing.T) {
	ctx := context.Background()
	o := options(t, &scripted{calls: [][2]string{{"bash", `{"command":"seq 1 30000; echo ERR"}`}}})
	o.Extensions = append(o.Extensions, big())
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := promptOn(ctx, s, "run it", nil); err != nil {
		t.Fatal(err)
	}
	var out string
	for _, it := range s.ag.State().Transcript {
		if o, ok := it.(*openresponses.FunctionCallOutput); ok {
			out = o.Output.Text
		}
	}
	ref := keptRef.FindString(out)
	if strings.Contains(out, "sidecar:") || strings.Contains(out, "sha256:") {
		t.Errorf("the cut result names the blob: %q", out[strings.Index(out, "left out")-40:][:300])
	}
	if len(out) > 31000 || !strings.Contains(out, "ERR\n[exit 0]") || !strings.HasPrefix(out, "1\n2\n") || ref == "" {
		i := strings.Index(out, "left out")
		t.Fatalf("bash told the model %d bytes, ref %q, note %q", len(out), ref, out[max(0, i-80):min(len(out), i+300)])
	}
	whole, err := s.ToolOutput(ctx, ref)
	if err != nil || !strings.Contains(string(whole), "\n15000\n") || !strings.HasSuffix(string(whole), "ERR\n[exit 0]") {
		t.Errorf("kept %d bytes, %v", len(whole), err)
	}
	rd, ok := s.Kit.LookupTool("read_output")
	if !ok {
		t.Fatal("no read_output")
	}
	res, err := rd.Execute(ctx, agenttool.Call{ID: "r", Args: json.RawMessage(`{"ref":"` + ref + `","pattern":"^1500[0-1]$"}`)})
	if err != nil || !strings.Contains(res.Output.Text, "15000\t15000") || !strings.Contains(res.Output.Text, "15001\t15001") {
		t.Errorf("read_output = %q, %v", res.Output.Text, err)
	}
	// Only an id this session kept: not a hash, not a made-up id, not
	// another session's (review R2-M1).
	for _, bad := range []string{"sidecar:sha256:" + strings.Repeat("0", 64), "out_0000000000000000"} {
		if _, err := s.ToolOutput(ctx, bad); err == nil {
			t.Errorf("read %q", bad)
		}
	}
	other, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.ToolOutput(ctx, ref); err == nil {
		t.Error("another session read this one's output")
	}
	// The record maps the id to the blob in an entry of its own, the
	// reference whole as its member, which keeps the blob and which a
	// redaction pass reads as a structural reference.
	var kept []string
	for _, e := range sessionEntries(t, s) {
		if c, ok := e.(*agentsession.CustomEntry); ok && c.NS == "dax.output_kept" {
			kept = append(kept, string(c.Data))
		}
	}
	if len(kept) != 1 || !strings.Contains(kept[0], `"id":"`+ref+`"`) || !regexp.MustCompile(`"blob":"sidecar:sha256:[0-9a-f]{64}"`).MatchString(kept[0]) {
		t.Errorf("dax.output_kept entries %q", kept)
	}

	// A tool that is not dax's own is cut at mcp_max_output.
	dump, ok := s.Kit.LookupTool("dump")
	if !ok {
		t.Fatal("no dump")
	}
	res, err = dump.Execute(ctx, agenttool.Call{ID: "d", Args: json.RawMessage(`{}`)})
	if err != nil || len(res.Output.Text) > 101000 || !strings.HasSuffix(res.Output.Text, "LAST") || keptRef.FindString(res.Output.Text) == "" {
		t.Errorf("dump told the model %d bytes, %v", len(res.Output.Text), err)
	}
}

// sessionEntries are s's entries as its store holds them now.
func sessionEntries(t *testing.T, s *Session) []agentsession.Entry {
	t.Helper()
	rd, ok := s.Recorder().Store().(agentsession.Reader)
	if !ok {
		t.Fatal("the store cannot be read")
	}
	sess, err := rd.Read(context.Background(), s.ID())
	if err != nil {
		t.Fatal(err)
	}
	return sess.Entries()
}
