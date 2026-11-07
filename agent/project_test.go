package agent

import (
	"context"
	"strings"
	"testing"

	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/openresponses/echo"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ChristopherDavenport/dax/ext/memory"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/internal/executor"
	"github.com/ChristopherDavenport/dax/tool"
)

// projectExecutor is an executor over dir that says its workspace is
// d and its project p (dax execute -project), dialed in memory.
func projectExecutor(t *testing.T, dir string, d workspace.Descriptor, p string) *Executor {
	t.Helper()
	ws, err := workspace.NewLocal(dir, tool.DefaultEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	srv, err := executor.NewServer(executor.ServeOptions{Name: "dax", Version: "box", Descriptor: d, Project: p},
		codingWith(acme()), extension.ToolEnv{Workspace: ws, Files: tool.NewFiles(ws)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	ex, err := connectExecutor(ctx, dialOnce(ct), executor.ConnectOptions{Name: "dax", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ex.Close() })
	return ex
}

// #155: a box's sessions record the image digest the box ran as the
// env entry's ref and its container as the instance, so a rebuild
// shows; the box's name stays the project's identity, so the two
// sessions are one project's and share one memory scope.
func TestABoxSessionRecordsItsImageAndKeepsItsName(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	o := options(t, &echo.Adapter{})
	var ids []string
	var scopes []string
	for _, run := range []struct{ image, instance string }{
		{"sha256:" + strings.Repeat("a", 64), "c0ffee000001"},
		{"sha256:" + strings.Repeat("b", 64), "c0ffee000002"},
	} {
		ex := projectExecutor(t, dir, workspace.Descriptor{Kind: workspace.KindContainer, Ref: run.image, Instance: run.instance, Host: "buildhost", Root: "/work"}, "dax-app-12345678")
		ro := remoteOptions(t, &echo.Adapter{}, ex)
		ro.Root, ro.UserDir = o.Root, o.UserDir
		s, err := New(ctx, ro)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := promptOn(ctx, s, "hello", nil); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, s.ID())
		scopes = append(scopes, string(memory.WorkspaceScope(s.ws.Descriptor(), ro.Dir)))
		if run.image == "sha256:"+strings.Repeat("b", 64) {
			sums, err := s.Sessions(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(sums) != 2 {
				t.Errorf("the box's project lists %d session(s), want both across the rebuild", len(sums))
			}
		}
		rec := string(projected(t, ro, s))
		for _, want := range []string{
			`"workspace":{"host":"buildhost","instance":"` + run.instance + `","kind":"container","ref":"` + run.image + `"}`,
			`"dax.workspace":{"host":"buildhost","instance":"` + run.instance + `","kind":"container","ref":"dax-app-12345678","root":"/work"}`,
		} {
			if !strings.Contains(rec, want) {
				t.Errorf("the record lacks %s:\n%s", want, firstLines(rec, 4))
			}
		}
	}
	if ids[0] == ids[1] {
		t.Errorf("both box runs recorded session %s, want two sessions", ids[0])
	}
	if scopes[0] != scopes[1] {
		t.Errorf("memory scopes %q and %q across a rebuild, want one", scopes[0], scopes[1])
	}
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
