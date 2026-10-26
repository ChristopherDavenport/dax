package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/agentworkspace/remote"
	"github.com/ChristopherDavenport/agentworkspace/tunnel"

	"github.com/ChristopherDavenport/dax/internal/authn"
	"github.com/ChristopherDavenport/dax/internal/config"
	"github.com/ChristopherDavenport/dax/policy"
	"github.com/ChristopherDavenport/dax/tool"
)

// A session whose workspace dialled in through a hub (workspace.hub, and
// `dax workspace -connect` on the workspace's host) acts there exactly
// as over a served workspace it dials: a write, a read and a bash call
// land in the directory the host serves, an MCP server starts there,
// and the record names the workspace by the tunnel's ref, never the
// token, by which -list finds it without dialing or listening.
func TestASessionThroughAHub(t *testing.T) {
	t.Setenv("DAX_TEST_MCP_SERVER", "1")
	ctx := context.Background()
	servedDir := t.TempDir()
	write(t, filepath.Join(servedDir, "README.md"), "# served\n")
	local, err := workspace.NewLocal(servedDir, append(tool.DefaultEnv(nil), "DAX_TEST_MCP_SERVER=1", "PLAIN=the workspace"))
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()

	// The session's end: the hub, which enrolls the one token as build.
	const token = "the-tunnel-token-of-at-least-32-bytes"
	hub, err := tunnel.NewHub(tunnel.WithEnrollment(authn.New(map[string]string{token: "build"}).Authenticator(errors.New("no such token"))))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(hub)
	defer srv.Close()
	defer hub.Close()
	// The workspace's end dials out to it.
	sctx, stop := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() {
		served <- tunnel.Serve(sctx, "ws"+strings.TrimPrefix(srv.URL, "http"), local,
			tunnel.WithScopes(remote.ScopeRead, remote.ScopeWrite, remote.ScopeExec),
			tunnel.WithHeader(func(context.Context) (http.Header, error) {
				return http.Header{"Authorization": {"Bearer " + token}}, nil
			}))
	}()
	defer func() {
		stop()
		select {
		case <-served:
		case <-time.After(10 * time.Second):
			t.Error("tunnel.Serve did not stop")
		}
	}()
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	ws, err := hub.Workspace(wctx, "build")
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	if _, ok := ws.(workspace.Starter); !ok {
		t.Fatal("a tunnel with exec over a Local is no Starter")
	}
	if d := ws.Descriptor(); d.Kind != workspace.KindRemote || d.Ref != config.TunnelRef("build") || d.Root != servedDir {
		t.Fatalf("the tunnel's descriptor: %+v", d)
	}

	model := &scripted{calls: [][2]string{
		{"write", `{"path":"made.txt","content":"hi\n"}`},
		{"read", `{"path":"` + servedDir + `/README.md"}`},
		{"bash", `{"command":"pwd > where.txt && cat made.txt"}`},
	}}
	o := options(t, model)
	o.Workspace = ws
	o.MCP = []MCPServer{{Name: "ws", Command: os.Args[0]}}
	o.Policy = &policy.Settings{Builtin: true, Fallback: "allow"}
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := promptOn(ctx, s, "go", nil); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(servedDir, "made.txt")); err != nil || string(b) != "hi\n" {
		t.Errorf("the write did not land in the served directory: %q %v", b, err)
	}
	if out := outputs(s); len(out) != 3 || !strings.Contains(out[1], "# served") || !strings.Contains(out[2], "hi") {
		t.Errorf("outputs %q", out)
	}
	if b, err := os.ReadFile(filepath.Join(servedDir, "where.txt")); err != nil || strings.TrimSpace(string(b)) != servedDir {
		t.Errorf("bash ran in %q, %v; want %s", b, err, servedDir)
	}
	where := leak(t, s, "mcp__ws__where")
	m := regexp.MustCompile(`dir=\[(.*)\] pid=\[(\d+)\]`).FindStringSubmatch(where)
	if m == nil || m[1] != servedDir {
		t.Fatalf("the server runs at %q, want %s", where, servedDir)
	}
	pid, _ := strconv.Atoi(m[2])

	id := s.ID()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the MCP server outlived its session")
		}
	}
	path, err := Project(ctx, o.Root, id, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"cwd":"` + servedDir + `"`, `"kind":"remote"`, `"ref":"http://build.tunnel.invalid"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("the record lacks %s", want)
		}
	}
	if strings.Contains(string(data), token) {
		t.Error("the record holds the token")
	}

	// -list finds it by the hub's name alone, and not by another name
	// or by the hub's address.
	if d, sums, err := ListWorkspaceURL(ctx, o.Root, config.TunnelRef("build")); err != nil || d != ws.Descriptor() || len(sums) != 1 || sums[0].Header.ID != id {
		t.Errorf("ListWorkspaceURL(the hub's ref) = %+v, %d sessions, %v", d, len(sums), err)
	}
	for _, other := range []string{config.TunnelRef("other"), srv.URL} {
		if d, sums, err := ListWorkspaceURL(ctx, o.Root, other); err != nil || d != (workspace.Descriptor{}) || len(sums) != 0 {
			t.Errorf("%s lists %+v %d, %v", other, d, len(sums), err)
		}
	}
}
