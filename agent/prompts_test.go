package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses/echo"
)

// A prompt template, the user's (~/.dax/prompts) or the project's
// (.dax/prompts), is expanded with its arguments and sent as a message
// the record says was composed from that file, the project's marked
// untrusted, so the record never reads it as the person's words
// (#168).
func TestAPromptTemplateIsSentAndRecordedAsComposed(t *testing.T) {
	ctx := context.Background()
	o := options(t, &echo.Adapter{})
	write(t, filepath.Join(o.UserDir, "prompts", "review.md"), "Review $ARGUMENTS for races; start with $1.\n")
	write(t, filepath.Join(o.Dir, ".dax", "prompts", "release.md"), "Write the changelog entry for $1.\n")
	// A name dax's own commands use, and one that is not a name.
	write(t, filepath.Join(o.UserDir, "prompts", "model.md"), "never\n")
	write(t, filepath.Join(o.UserDir, "prompts", "Bad Name.md"), "never\n")
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range s.Templates() {
		names = append(names, p.Name+map[bool]string{true: " (project)", false: ""}[p.Project])
	}
	if strings.Join(names, ",") != "release (project),review" {
		t.Errorf("templates %q", names)
	}
	if _, err := s.Template("model", ""); err == nil {
		t.Error("a template named as a command was taken")
	}
	item, err := s.Template("review", "internal/box tool")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Drive(ctx, s, rulesOf(nil), item); err != nil {
		t.Fatal(err)
	}
	proj, err := s.Template("release", "v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Drive(ctx, s, rulesOf(nil), proj); err != nil {
		t.Fatal(err)
	}
	data := string(projected(t, o, s))
	for _, want := range []string{
		"Review internal/box tool for races; start with internal/box.",
		`"instructions_part":{"id":"~/.dax/prompts/review.md","source":"dax.prompts"}`,
		"Write the changelog entry for v0.1.0.",
		`"instructions_part":{"id":".dax/prompts/release.md","source":"dax.prompts"}`,
		`"untrusted"`,
	} {
		if !strings.Contains(data, want) {
			t.Errorf("the record lacks %s", want)
		}
	}
}
