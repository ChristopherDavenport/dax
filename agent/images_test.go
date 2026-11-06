package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/agentworkspace/remote"
	"github.com/ChristopherDavenport/openresponses"
)

// pngBytes is a small PNG.
func pngBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 3, 2))
	img.Set(1, 1, color.RGBA{R: 255, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// imageSeen is scripted, keeping each request's input as JSON.
type imageSeen struct {
	scripted
	mu   sync.Mutex
	reqs []string
}

func (m *imageSeen) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	var b strings.Builder
	for _, it := range req.Input {
		data, _ := json.Marshal(it)
		b.Write(data)
		b.WriteString("\n")
	}
	m.mu.Lock()
	m.reqs = append(m.reqs, b.String())
	m.mu.Unlock()
	return m.scripted.CreateStream(ctx, req, sink)
}

func (m *imageSeen) last() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reqs[len(m.reqs)-1]
}

// #166: a PNG read on a served workspace reaches a model that takes
// images as an image; the store keeps it as a blob and the log names it
// by a sidecar: reference; the resumed session's request carries the
// image again; a model that does not take images is told so instead.
func TestAReadImageIsKeptAsABlobAndResumed(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	data := pngBytes(t)
	if err := os.WriteFile(filepath.Join(dir, "shot.png"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	local, err := workspace.NewLocal(dir, servedEnv())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { local.Close() })
	ws := serveWS(t, local, remote.ScopeRead, remote.ScopeWrite, remote.ScopeExec)
	url := "data:image/png;base64,"

	m := &imageSeen{scripted: scripted{calls: [][2]string{{"read", `{"path":"shot.png"}`}}}}
	o := options(t, m)
	o.Workspace = ws
	o.TakesImages = func(_ context.Context, model string) bool { return model == "echo" }
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := promptOn(ctx, s, "what is wrong in the screenshot?", nil); err != nil {
		t.Fatal(err)
	}
	if got := m.last(); !strings.Contains(got, url) || !strings.Contains(got, `"input_image"`) {
		t.Fatalf("the model was not sent the image:\n%s", got)
	}
	id := s.ID()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := cas.Open(o.Root, cas.WithReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	blob, err := st.Blob(ctx, agentsession.HashBytes(data))
	st.Close()
	if err != nil || !bytes.Equal(blob, data) {
		t.Fatalf("the store's blob: %d bytes, %v", len(blob), err)
	}
	if n, failed, err := Verify(ctx, o.Root, id); err != nil || len(failed) != 0 || n == 0 {
		t.Errorf("verify: %d, %v, %v", n, failed, err)
	}
	path, err := Project(ctx, o.Root, id, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	log, _ := os.ReadFile(path)
	if !strings.Contains(string(log), "sidecar:") || strings.Contains(string(log), url) || !strings.Contains(string(log), `"media":"sidecar"`) {
		t.Errorf("the log holds the image inline, or no sidecar reference:\n%s", log)
	}

	m.scripted.calls = nil
	r, err := Resume(ctx, o, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := promptOn(ctx, r, "and now?", nil); err != nil {
		t.Fatal(err)
	}
	if got := m.last(); !strings.Contains(got, url) || strings.Contains(got, "sidecar:") {
		t.Errorf("the resumed request does not carry the image:\n%s", got)
	}
	r.Close()

	// A model that does not take images is told so; the read still runs.
	n := &imageSeen{scripted: scripted{calls: [][2]string{{"read", `{"path":"shot.png"}`}}}}
	o2 := options(t, n)
	o2.Workspace = ws
	s2, err := New(ctx, o2)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := promptOn(ctx, s2, "look", nil); err != nil {
		t.Fatal(err)
	}
	if got := n.last(); strings.Contains(got, url) || !strings.Contains(got, "does not take images") {
		t.Errorf("a model that takes no images was sent:\n%s", got)
	}
	if _, err := s2.Image(ctx, filepath.Join(dir, "shot.png")); err == nil {
		t.Error("an attachment for a model that takes no images was made")
	}
}

// The person's @path.png is read on this machine, refused unless it is
// the image its name says and within the size.
func TestAnAttachedImage(t *testing.T) {
	ctx := context.Background()
	o := options(t, &scripted{})
	o.TakesImages = func(context.Context, string) bool { return true }
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	dir := t.TempDir()
	good, fake := filepath.Join(dir, "a.PNG"), filepath.Join(dir, "b.png")
	os.WriteFile(good, pngBytes(t), 0o644)
	os.WriteFile(fake, []byte("not a png"), 0o644)
	img, err := s.Image(ctx, good)
	if err != nil || !strings.HasPrefix(img.ImageURL, "data:image/png;base64,") {
		t.Fatalf("image %v, %v", img, err)
	}
	for _, p := range []string{fake, filepath.Join(dir, "missing.png"), filepath.Join(dir, "notes.txt"), dir + "/"} {
		if _, err := s.Image(ctx, p); err == nil {
			t.Errorf("%s was attached", p)
		}
	}
}

// An image read in an executor reaches the session as the image too,
// through the executor's MCP.
func TestAnImageReadInAnExecutor(t *testing.T) {
	ctx := context.Background()
	box := newRemoteBox(t)
	if err := os.WriteFile(filepath.Join(box.dir, "shot.png"), pngBytes(t), 0o644); err != nil {
		t.Fatal(err)
	}
	ex, _ := box.dial(t)
	m := &imageSeen{scripted: scripted{calls: [][2]string{{"read", `{"path":"shot.png"}`}}}}
	o := remoteOptions(t, m, ex)
	o.TakesImages = func(context.Context, string) bool { return true }
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := promptOn(ctx, s, "look", func(*openresponses.FunctionCall, string) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if got := m.last(); !strings.Contains(got, "data:image/png;base64,") {
		t.Errorf("the model was not sent the executor's image:\n%s", got)
	}
}
