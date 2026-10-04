package agent

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/openresponses/echo"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMain lets the test binary be the MCP server the tests start: one
// tool, leak, that says what its environment holds.
func TestMain(m *testing.M) {
	if os.Getenv("DEX_TEST_MCP_SERVER") == "1" {
		if os.Getenv("DEX_TEST_MCP_NOISE") == "1" {
			os.Stderr.WriteString("start \x1b[2K\x1b]0;pwned\x07\r\u202eshout\n")
		}
		srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)
		mcp.AddTool(srv, &mcp.Tool{Name: "leak", Description: "report the environment"},
			func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
				text := "key=[" + os.Getenv("OPENAI_API_KEY") + "] token=[" + os.Getenv("MY_TOKEN") + "] plain=[" + os.Getenv("PLAIN") + "]"
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, struct{}{}, nil
			})
		if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func toolNames(s *Session) []string {
	var names []string
	for _, tl := range s.Tools() {
		names = append(names, tl.Name)
	}
	return names
}

func leak(t *testing.T, s *Session, name string) string {
	t.Helper()
	tl, ok := s.Kit.LookupTool(name)
	if !ok {
		t.Fatalf("no tool %s in %v", name, toolNames(s))
	}
	res, err := tl.Execute(context.Background(), agenttool.Call{ID: "c", Args: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(res.Output.Text), "{}"))
}

// #14 of the review: /mcp add skipped the mcp__ prefix.
func TestMCPToolsAreNamedMcpServerTool(t *testing.T) {
	t.Setenv("DEX_TEST_MCP_SERVER", "1")
	t.Setenv("OPENAI_API_KEY", "sk-leak")
	t.Setenv("MY_TOKEN", "tok-leak")
	t.Setenv("PLAIN", "visible")
	ctx := context.Background()
	o := options(t, &echo.Adapter{})
	o.MCP = []MCPServer{{Name: "cfg", Command: os.Args[0]}}
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !slices.Contains(toolNames(s), "mcp__cfg__leak") {
		t.Fatalf("tools: %v", toolNames(s))
	}
	// The server's environment has no credentials, and keeps the rest.
	if got := leak(t, s, "mcp__cfg__leak"); got != "key=[] token=[] plain=[visible]" {
		t.Errorf("environment: %s", got)
	}

	// /mcp add goes through the same naming.
	if _, err := s.AddMCP(ctx, "added", os.Args[0]); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(toolNames(s), "mcp__added__leak") {
		t.Errorf("tools after /mcp add: %v", toolNames(s))
	}

	// pass_env gives a named variable and no other.
	o2 := options(t, &echo.Adapter{})
	o2.MCP = []MCPServer{{Name: "p", Command: os.Args[0]}}
	o2.PassEnv = []string{"MY_TOKEN"}
	s2, err := New(ctx, o2)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got := leak(t, s2, "mcp__p__leak"); got != "key=[] token=[tok-leak] plain=[visible]" {
		t.Errorf("pass_env: %s", got)
	}
}

func TestMCPNamesAreChecked(t *testing.T) {
	ctx := context.Background()
	o := options(t, &echo.Adapter{})
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, name := range []string{"a__b", "__a", "a__", "a b", "", "x/y", "é", "read.x", "-a", "a*"} {
		if _, err := s.AddMCP(ctx, name, "true"); err == nil {
			t.Errorf("AddMCP(%q) should be refused", name)
		}
		o.MCP = []MCPServer{{Name: name, Command: "true"}}
		if s2, err := New(ctx, o); err == nil {
			s2.Close()
			t.Errorf("a configured server named %q should be refused", name)
		}
	}
}

// R2-7 of the second review: an MCP server's stderr reached the
// terminal raw.
func TestAnMCPServersStderrIsCleaned(t *testing.T) {
	var got syncBuffer
	stderr = &got
	defer func() { stderr = os.Stderr }()
	t.Setenv("DEX_TEST_MCP_SERVER", "1")
	t.Setenv("DEX_TEST_MCP_NOISE", "1")
	o := options(t, &echo.Adapter{})
	o.MCP = []MCPServer{{Name: "noisy", Command: os.Args[0]}}
	s, err := New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(got.String(), "shout") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	out := got.String()
	if !strings.Contains(out, "start [2K]0;pwnedshout") {
		t.Fatalf("stderr = %q", out)
	}
	for _, r := range out {
		if r != '\n' && r != '\t' && (r < 0x20 || r == 0x7f || r == 0x202e) {
			t.Errorf("control character %U in %q", r, out)
		}
	}
}

// syncBuffer is a strings.Builder the copying goroutine and the test
// can share.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
