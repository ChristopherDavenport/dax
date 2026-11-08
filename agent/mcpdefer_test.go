package agent

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/ext/toolsearch"
)

// sized is a tool whose definition is about n bytes.
func sized(name string, n int) agenttool.Tool {
	return agenttool.New(name, strings.Repeat("d", n), func(context.Context, agenttool.NoArgs) (string, error) { return "", nil })
}

// A server whose config says nothing is held back once its definitions
// pass MCPDeferBytes; one that says defer is held back or not as it
// says, whatever its size; the answer is kept for the session; and
// nothing but an MCP server is ever held.
func TestDeferMCPDecidesBySizeUnlessTheConfigSays(t *testing.T) {
	yes, no := true, false
	s := &Session{servers: []MCPServerInfo{
		{Name: "small"}, {Name: "big"}, {Name: "bigoff", deferSet: &no}, {Name: "smallon", deferSet: &yes},
	}}
	small := []agenttool.Tool{sized("mcp__small__a", 100), sized("mcp__small__b", 100)}
	big := []agenttool.Tool{sized("mcp__big__a", MCPDeferBytes/2), sized("mcp__big__b", MCPDeferBytes/2)}
	for _, tc := range []struct {
		name  string
		tools []agenttool.Tool
		want  bool
	}{
		{"small", small, false},
		{"big", big, true},
		{"bigoff", []agenttool.Tool{sized("mcp__bigoff__a", 2*MCPDeferBytes)}, false},
		{"smallon", []agenttool.Tool{sized("mcp__smallon__a", 10)}, true},
	} {
		if got := s.deferMCP("mcp:#1 x", tc.tools); got != tc.want {
			t.Errorf("%s: deferMCP = %v, want %v (%d bytes)", tc.name, got, tc.want, toolDefinitionBytes(tc.tools))
		}
	}
	// Decided once: the big server's list shrinking leaves it held.
	if !s.deferMCP("mcp:#1 x", big[:1]) {
		t.Error("big moved out of the listing when its list changed")
	}
	if s.deferMCP("WithTools", big) || s.deferMCP("mcp:#9 y", []agenttool.Tool{sized("mcp__unknown__a", 2*MCPDeferBytes)}) {
		t.Error("a source that is no server of the session's was held")
	}
	if !s.deferDecided("big") || s.deferDecided("small") {
		t.Error("deferDecided does not say what deferMCP decided")
	}
}

// requestLog records each request's tool names, before a scripted model
// answers it.
type requestLog struct {
	*scripted
	mu    sync.Mutex
	tools [][]string
}

func (m *requestLog) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	var names []string
	for _, tl := range req.Tools {
		if f, ok := tl.(*openresponses.FunctionTool); ok {
			names = append(names, f.Name)
		}
	}
	m.mu.Lock()
	m.tools = append(m.tools, names)
	m.mu.Unlock()
	return m.scripted.CreateStream(ctx, req, sink)
}

// With dax-toolsearch, a server the config defers is listed to the
// model and not sent; tool_search runs unasked and loads a tool, which
// the next request carries after the tools already sent; and the loaded
// tool's call is still decided by the policy, which asks. Info and the
// listings say which servers are held and what is loaded.
func TestADeferredServersToolIsLoadedAndStillAsks(t *testing.T) {
	t.Setenv("DAX_TEST_MCP_SERVER", "1")
	yes := true
	model := &requestLog{scripted: &scripted{calls: [][2]string{
		{agentkit.ToolSearchTool, `{"query":"select:mcp__held__where"}`},
		{"mcp__held__where", `{}`},
	}}}
	o := options(t, model)
	o.Extensions = append(o.Extensions, toolsearch.New())
	o.Policy = confirmPolicy(t)
	o.MCP = []MCPServer{{Name: "sent", Command: os.Args[0]}, {Name: "held", Command: os.Args[0], Defer: &yes}}
	s, err := New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	end, err := s.Prompt(context.Background(), openresponses.UserText("where does it run?"))
	if err != nil {
		t.Fatal(err)
	}
	if end.Reason != agentturn.ReasonInputRequired || len(end.Pending) != 1 || end.Pending[0].Call.Name != "mcp__held__where" {
		t.Fatalf("end = %q, %d pending; want the loaded tool's call asked about", end.Reason, len(end.Pending))
	}
	model.mu.Lock()
	reqs := slices.Clone(model.tools)
	model.mu.Unlock()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want 2", len(reqs))
	}
	first, second := reqs[0], reqs[1]
	if !slices.Contains(first, "mcp__sent__where") || slices.ContainsFunc(first, func(n string) bool { return strings.HasPrefix(n, "mcp__held__") }) {
		t.Errorf("first request tools %v: want sent's tools and none of held's", first)
	}
	if first[len(first)-1] != agentkit.ToolSearchTool {
		t.Errorf("first request tools %v: want tool_search last", first)
	}
	if want := append(slices.Clone(first), "mcp__held__where"); !slices.Equal(second, want) {
		t.Errorf("second request tools = %v, want the first's then the loaded tool", second)
	}

	info := s.Info()
	if !slices.Equal(info.Loaded, []string{"mcp__held__where"}) {
		t.Errorf("Info.Loaded = %v", info.Loaded)
	}
	for _, m := range info.MCP {
		switch m.Name {
		case "held":
			if m.Deferred == 0 || m.Loaded != 1 {
				t.Errorf("held listed as %+v, want deferred with one loaded", m)
			}
		case "sent":
			if m.Deferred != 0 || m.Loaded != 0 {
				t.Errorf("sent listed as %+v, want nothing deferred", m)
			}
		}
	}
	data, err := json.Marshal(info.MCP)
	if err != nil || !strings.Contains(string(data), `"Deferred"`) {
		t.Errorf("the served info lacks the deferred count: %s %v", data, err)
	}
}
