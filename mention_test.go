package dax

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentconsole/console"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/agent"
)

// fakeMentions is controls whose servers offer resources.
type fakeMentions struct {
	fakeControls
	listed int
}

func (f *fakeMentions) MCPResources(context.Context) []agent.MCPResource {
	f.listed++
	return []agent.MCPResource{
		{Mention: "@team:file:///notes.txt", Name: "notes", Description: "the team's\nnotes \x1b[31m"},
		{Mention: "@team:file:///\x1b]0;x\x07", Name: "evil"},
		{Mention: "@wiki:page/1", Name: "page"},
	}
}

func (f *fakeMentions) MCPMentions(_ context.Context, text string) (openresponses.Items, error) {
	if strings.Contains(text, "@broken:") {
		return nil, errors.New("@broken:x: \x1b[2Jgone")
	}
	if strings.Contains(text, "@team:") {
		return openresponses.Items{openresponses.UserText("<mcp_resource/>")}, nil
	}
	return nil, nil
}

// A typed line is sent with the resources it mentions after it; a
// mention that cannot be read is an error, cleaned, and nothing is
// sent. Over dax attach the line goes as typed (#176).
func TestATypedLineCarriesItsMentions(t *testing.T) {
	ctx := context.Background()
	f := &fakeMentions{}
	items, err := typedItems(ctx, f, "see @team:file:///notes.txt")
	if err != nil || len(items) != 2 {
		t.Fatalf("items %v, err %v", items, err)
	}
	if _, err := typedItems(ctx, f, "@broken:x"); err == nil || strings.Contains(err.Error(), "\x1b") {
		t.Errorf("a failed read: %q", err)
	}
	if items, _ := typedItems(ctx, &fakeControls{}, "see @team:file:///notes.txt"); len(items) != 1 {
		t.Errorf("over attach %d items", len(items))
	}
	// The terminal client's submit sends the same.
	sub, err := submitLine(nil, f, func() {})(ctx, console.Line{Text: "see @team:file:///notes.txt"})
	if err != nil || len(sub.Prompt) != 2 {
		t.Errorf("submit %+v, %v", sub, err)
	}
}

// @ completes the servers' resources by prefix, the list asked again
// only after a while; a mention carrying a terminal's controls is left
// out, and help is one cleaned line.
func TestAtCompletesTheServersResources(t *testing.T) {
	ctx := context.Background()
	f := &fakeMentions{}
	complete := mentionCompletion(f)
	if got := complete(ctx, console.Completing{Word: "notes"}); got != nil {
		t.Errorf("a word without @: %v", got)
	}
	got := complete(ctx, console.Completing{Word: "@te"})
	if len(got) != 1 || got[0].Text != "@team:file:///notes.txt" || strings.ContainsAny(got[0].Help, "\n\x1b") {
		t.Fatalf("completions %+v", got)
	}
	if got := complete(ctx, console.Completing{Word: "@"}); len(got) != 2 {
		t.Errorf("@ alone: %+v", got)
	}
	if f.listed != 1 {
		t.Errorf("listed %d times, want once while fresh", f.listed)
	}
}
