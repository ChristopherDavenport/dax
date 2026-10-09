package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession/cas"
	workspace "github.com/ChristopherDavenport/agentworkspace"

	"github.com/ChristopherDavenport/dax/ext/coding"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/policy"
)

// boxed is a workspace that is a container to the session: its root is
// /workspace and it says so, while its files are a directory here. It
// stands in for a container or a remote runtime, which implement the
// same interface.
type boxed struct {
	*workspace.Local
	closed bool
}

func (b *boxed) Root() string { return "/workspace" }
func (b *boxed) Descriptor() workspace.Descriptor {
	return workspace.Descriptor{Kind: workspace.KindContainer, Ref: "sha256:abc", Root: "/workspace"}
}
func (b *boxed) Close() error { b.closed = true; return b.Local.Close() }

// A session over a workspace that is not this machine works as one
// over this machine does: the model is told the workspace's root and
// its paths are paths in it, the tools act there, and the record names
// the workspace by its kind, ref and root. The store is the caller's,
// as a remote store would be; the session leaves it and the workspace
// open for the caller to close.
func TestASessionInAContainerIsRecordedAsOne(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	files := filepath.Join(base, "box")
	if err := os.MkdirAll(files, 0o755); err != nil {
		t.Fatal(err)
	}
	local, err := workspace.NewLocal(files, nil)
	if err != nil {
		t.Fatal(err)
	}
	ws := &boxed{Local: local}
	t.Cleanup(func() { ws.Close() })
	root := filepath.Join(base, "sessions")
	store, err := cas.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	model := &scripted{calls: [][2]string{{"write", `{"path":"/workspace/hello.txt","content":"hi\n"}`}}}
	o := Options{
		Model: "echo", Streamer: model, Dir: filepath.Join(base, "checkout"), UserDir: filepath.Join(base, "user"),
		Workspace:  ws,
		Store:      store,
		Extensions: []extension.Extension{coding.New(0)},
		Policy:     &policy.Settings{Builtin: true, Fallback: "allow"},
	}
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.systemPrompt(), "Current working directory: /workspace") {
		t.Errorf("the model is not told the workspace's root:\n%s", s.systemPrompt())
	}
	if _, err := promptOn(ctx, s, "write it", nil); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(files, "hello.txt")); err != nil || string(b) != "hi\n" {
		t.Errorf("the write did not land in the workspace: %q %v", b, err)
	}
	if out := outputs(s); len(out) != 1 || !strings.Contains(out[0], "wrote 3 bytes to hello.txt") {
		t.Errorf("outputs %q", out)
	}
	id := s.ID()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if ws.closed {
		t.Error("the session closed the caller's workspace")
	}
	if _, err := store.Open(ctx, id); err != nil {
		t.Errorf("the session closed the caller's store: %v", err)
	}
	store.Close()

	path, err := Project(ctx, root, id, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"cwd":"/workspace"`, `"kind":"container"`, `"ref":"sha256:abc"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("the record lacks %s", want)
		}
	}
	if strings.Contains(string(data), `"kind":"local"`) {
		t.Error("the record calls the container local")
	}
}
