package agent

import (
	"context"
	"strings"
	"testing"

	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/openresponses/echo"
)

// The header records the workspace's host and instance, so a session
// resumed on another machine, or in a new container, says it moved,
// though the root is the same path; a header from before records
// neither and compares by kind, ref and root. The project's identity,
// which -list goes by, is still kind, ref and root (review M2).
func TestAResumeOnAnotherMachineSaysSo(t *testing.T) {
	d := workspace.Descriptor{Kind: workspace.KindLocal, Root: "/src/p", Host: "a"}
	for _, tc := range []struct {
		name string
		was  workspace.Descriptor
		same bool
	}{
		{"the same machine", d, true},
		{"another machine", workspace.Descriptor{Kind: workspace.KindLocal, Root: "/src/p", Host: "b"}, false},
		{"a header from before", workspace.Descriptor{Kind: workspace.KindLocal, Root: "/src/p"}, true},
		{"a new container", workspace.Descriptor{Kind: workspace.KindContainer, Ref: "box", Root: "/w", Instance: "c1"}, false},
	} {
		now := d
		if tc.name == "a new container" {
			now = workspace.Descriptor{Kind: workspace.KindContainer, Ref: "box", Root: "/w", Instance: "c2"}
		}
		if got := sameWorkspace(tc.was, now); got != tc.same {
			t.Errorf("%s: same = %v", tc.name, got)
		}
		if !sameProject(tc.was, now) {
			t.Errorf("%s: not one project", tc.name)
		}
	}

	ctx := context.Background()
	o := options(t, &echo.Adapter{})
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	host := s.x.Descriptor().Host
	if _, err := promptOn(ctx, s, "hi", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(projected(t, o, s)), `"host":"`+host+`"`) {
		t.Error("the header does not record the host")
	}
	sums, err := List(ctx, o.Root, o.Dir)
	if err != nil || len(sums) != 1 {
		t.Errorf("listed %d session(s), %v; want the one, its host aside", len(sums), err)
	}
}
