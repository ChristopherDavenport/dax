package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
	"github.com/ChristopherDavenport/openresponses/echo"
)

// #164: Reopen with an ID resumes that session over the live settings;
// this session's own ID is refused.
func TestReopenResumesAnotherSession(t *testing.T) {
	ctx := context.Background()
	o := options(t, &echo.Adapter{})
	a, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := promptOn(ctx, a, "the auth refactor", nil); err != nil {
		t.Fatal(err)
	}
	first := a.ID()
	b, err := a.Reopen(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	if _, err := b.Reopen(ctx, b.ID()); err == nil {
		t.Error("resuming the session itself should be refused")
	}
	if err := b.SetModel("other"); err != nil {
		t.Fatal(err)
	}
	c, err := b.Reopen(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	b.Close()
	defer c.Close()
	if c.ID() != first {
		t.Fatalf("resumed %q, want %q", c.ID(), first)
	}
	if c.Model() != "other" {
		t.Errorf("model %q after resume, want the live other", c.Model())
	}
	if _, err := promptOn(ctx, c, "go on", nil); err != nil {
		t.Fatal(err)
	}
	items := openresponses.Items(c.ag.State().Transcript)
	if len(items) < 3 || !strings.Contains(itemsText(items), "the auth refactor") {
		t.Errorf("resumed transcript %q, want the earlier conversation's head", itemsText(items))
	}
}

// #164: /name writes an info entry the session's name reads back and
// this project's listing shows; a name is one line, not empty.
func TestASessionsNameIsListed(t *testing.T) {
	ctx := context.Background()
	o := options(t, &echo.Adapter{})
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, bad := range []string{"", "  ", "two\nlines", "bell\a", strings.Repeat("x", 201)} {
		if err := s.SetName(ctx, bad); err == nil {
			t.Errorf("SetName(%q) should be refused", bad)
		}
	}
	if err := s.SetName(ctx, "auth refactor"); err != nil {
		t.Fatal(err)
	}
	if s.Name() != "auth refactor" || s.Info().Name != "auth refactor" {
		t.Errorf("name %q, info %q", s.Name(), s.Info().Name)
	}
	// Elsewhere in the store: a session in another project.
	other := options(t, &echo.Adapter{})
	other.Root = o.Root
	x, err := New(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	x.Close()
	sums, err := s.Sessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 1 || sums[0].Header.ID != s.ID() || sums[0].Name != "auth refactor" {
		t.Errorf("sessions %+v, want this one alone, named", sums)
	}
	listed, err := List(ctx, o.Root, o.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Name != "auth refactor" {
		t.Errorf("-list's sessions %+v, want the name", listed)
	}
}
