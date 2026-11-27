package dax

import (
	"context"
	"errors"
	"slices"
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

// A slash that starts the input lists the commands, narrowed by what is
// typed: dax's own, each once with its plainest form's help, then the
// session's prompt templates and its servers' prompts. A command that
// takes arguments completes with a space after it. A slash later in the
// input, or two of them, is the model's text.
func TestSlashCompletesTheCommands(t *testing.T) {
	ctx := context.Background()
	complete := commandCompletion(&templateControls{})
	texts := func(word string) []string {
		var out []string
		for _, c := range complete(ctx, console.Completing{Before: word, Word: word}) {
			out = append(out, c.Text)
		}
		return out
	}
	all := texts("/")
	for _, want := range []string{"/help", "/model ", "/mcp", "/clear", "/quit", "/exit", "/review ", "/mcp__team__review_pr "} {
		if !slices.Contains(all, want) {
			t.Errorf("a lone slash lacks %q: %q", want, all)
		}
	}
	if n := len(all); n != len(slices.Compact(slices.Sorted(slices.Values(all)))) {
		t.Errorf("a command listed twice: %q", all)
	}
	if got := texts("/mo"); !slices.Equal(got, []string{"/model ", "/mode "}) {
		t.Errorf("/mo completes %q", got)
	}
	got := complete(ctx, console.Completing{Before: "/mcp", Word: "/mcp"})
	if len(got) != 2 || got[0].Text != "/mcp" || got[0].Help != "the MCP servers, where each runs and what it is for" ||
		got[1].Help != "<number>  review a pull request" {
		t.Errorf("/mcp completes %+v", got)
	}
	if got := complete(ctx, console.Completing{Before: "/think", Word: "/think"}); len(got) != 1 || got[0].Help != "on|off  reasoning from the next request" {
		t.Errorf("/think completes %+v", got)
	}
	for _, c := range []console.Completing{
		{Before: "see /mo", Word: "/mo"},
		{Before: "first\n/mo", Word: "/mo"},
		{Before: "//etc", Word: "//etc"},
		{Before: "mo", Word: "mo"},
	} {
		if got := complete(ctx, c); got != nil {
			t.Errorf("%q completes %+v", c.Before, got)
		}
	}
	// Over dax attach the controls offer no templates and no prompts.
	if got := commandCompletion(&fakeControls{})(ctx, console.Completing{Before: "/re", Word: "/re"}); len(got) != 1 || got[0].Text != "/resume " {
		t.Errorf("over attach /re completes %+v", got)
	}
	// Every command the list completes is one dax runs.
	for _, c := range commandList {
		if !strings.HasPrefix(c.name, "/") || strings.HasPrefix(c.name, "//") {
			continue
		}
		if _, err := command(ctx, nil, &fakeControls{}, c.name, false); err != nil && strings.HasPrefix(err.Error(), "unknown command") {
			t.Errorf("%s is listed and unknown", c.name)
		}
	}
}
