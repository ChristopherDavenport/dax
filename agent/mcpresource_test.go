package agent

import (
	"context"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ChristopherDavenport/openresponses"
	"github.com/ChristopherDavenport/openresponses/echo"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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

// A word is a mention only of a server the session has, so a path that
// happens to hold a colon is not one (#176, #166).
func TestIsMCPMention(t *testing.T) {
	t.Setenv("DAX_TEST_MCP_SERVER", "1")
	ctx := context.Background()
	o := options(t, &echo.Adapter{})
	o.MCP = []MCPServer{{Name: "team", Command: os.Args[0]}}
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for word, want := range map[string]bool{
		"@team:file:///shot.png": true,
		"@team:shot.png":         true,
		"@nobody:shot.png":       false,
		"@shot.png":              false,
		"team:shot.png":          false,
	} {
		if got := s.IsMCPMention(word); got != want {
			t.Errorf("IsMCPMention(%q) = %v, want %v", word, got, want)
		}
	}
}

// A resource's text is cut on a rune's first byte, at 100 KiB each and
// at 256 KiB for all a line's mentions attach.
func TestAResourceIsCutOnARuneAndALineIsCapped(t *testing.T) {
	big := strings.Repeat("é", maxResource) // two bytes each
	left := maxMentioned
	var texts []string
	for range 4 {
		texts = append(texts, resourceText("team", "file:///big", []*mcp.ResourceContents{{Text: big}}, &left))
	}
	for i, txt := range texts {
		if !utf8.ValidString(txt) {
			t.Errorf("attachment %d is not whole runes", i)
		}
		if !strings.Contains(txt, "[cut: ") {
			t.Errorf("attachment %d says no cut", i)
		}
	}
	var sum int
	for _, txt := range texts {
		sum += strings.Count(txt, "é") * 2
	}
	if sum > maxMentioned || sum < maxMentioned-4 {
		t.Errorf("the line attached %d bytes of text, want at most %d", sum, maxMentioned)
	}
	if !strings.Contains(texts[2], "a line attaches at most") || !strings.Contains(texts[3], "[cut: 0 of ") {
		t.Errorf("the line's cap is not said:\n%.200s\n%.200s", texts[2][len(texts[2])-200:], texts[3])
	}
}
