package agent

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses/echo"
)

// An MCP server's prompt is a command, /mcp__<server>__<prompt> args,
// its arguments the declared ones in order, the last taking the rest;
// its messages are sent marked as the server's prompt and untrusted,
// never as the person's words (#176).
func TestAnMCPPromptIsSentAsTheServers(t *testing.T) {
	t.Setenv("DAX_TEST_MCP_SERVER", "1")
	ctx := context.Background()
	o := options(t, &echo.Adapter{})
	o.MCP = []MCPServer{{Name: "team", Command: os.Args[0]}}
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	prompts := s.MCPPrompts(ctx)
	if len(prompts) != 1 || prompts[0].Command != "mcp__team__review_pr" || prompts[0].Description != "review a pull request" || strings.Join(prompts[0].Arguments, ",") != "number,focus" {
		t.Fatalf("prompts %+v", prompts)
	}
	items, err := s.MCPPrompt(ctx, "mcp__team__review_pr", "456 the error paths")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MCPPrompt(ctx, "mcp__team__nope", ""); err == nil {
		t.Error("a prompt the server does not offer was taken")
	}
	if _, err := Drive(ctx, s, rulesOf(nil), items...); err != nil {
		t.Fatal(err)
	}
	data := string(projected(t, o, s))
	for _, want := range []string{
		"Review PR 456 for the error paths.",
		`"instructions_part":{"id":"mcp__team__review_pr","source":"dax.mcp_prompt"}`,
		`"untrusted"`,
	} {
		if !strings.Contains(data, want) {
			t.Errorf("the record lacks %s", want)
		}
	}
}
