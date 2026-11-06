package agent

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
	"github.com/ChristopherDavenport/openresponses/echo"
)

// An @<server>:<uri> mention attaches the server's resource once,
// however often the line names it, marked as the resource and
// untrusted, never as the person's words; a name no server has is text
// (#176).
func TestAnMCPResourceMentionIsAttachedAsTheServers(t *testing.T) {
	t.Setenv("DAX_TEST_MCP_SERVER", "1")
	ctx := context.Background()
	o := options(t, &echo.Adapter{})
	o.MCP = []MCPServer{{Name: "team", Command: os.Args[0]}}
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	rs := s.MCPResources(ctx)
	if len(rs) != 1 || rs[0].Mention != "@team:file:///notes.txt" || rs[0].Name != "notes" || rs[0].Description != "the team's notes" {
		t.Fatalf("resources %+v", rs)
	}
	line := "when do we deploy? see @team:file:///notes.txt, and again @team:file:///notes.txt. mail me@example.com:x or @nobody:file:///x"
	items, err := s.MCPMentions(ctx, line)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("%d item(s) attached, want the one resource once", len(items))
	}
	if _, err := s.MCPMentions(ctx, "@team:file:///missing.txt"); err == nil {
		t.Error("a resource the server cannot read was attached")
	}
	if none, err := s.MCPMentions(ctx, "no mention here"); err != nil || len(none) != 0 {
		t.Errorf("a line with no mention: %v, %v", none, err)
	}
	all := append(openresponses.Items{openresponses.UserText(line)}, items...)
	if _, err := Drive(ctx, s, rulesOf(nil), all...); err != nil {
		t.Fatal(err)
	}
	data := string(projected(t, o, s))
	for _, want := range []string{
		"Deploys happen on Tuesdays.",
		`<mcp_resource server=\"team\" uri=\"file:///notes.txt\">`,
		`"instructions_part":{"id":"team:file:///notes.txt","source":"dax.mcp_resource"}`,
		`"untrusted"`,
	} {
		if !strings.Contains(data, want) {
			t.Errorf("the record lacks %s", want)
		}
	}
}
