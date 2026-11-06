package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/openresponses"
)

// #166: read returns an image file as an image part, with a line saying
// what it is, through the workspace's files; one whose bytes are not
// its name's type is read as text, and one over the size is said.
func TestReadReturnsAnImage(t *testing.T) {
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewGray(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "shot.png"), b.Bytes(), 0o644)
	os.WriteFile(filepath.Join(dir, "fake.png"), []byte("just text\n"), 0o644)
	big := append([]byte("\xff\xd8\xff"), make([]byte, MaxImageBytes)...)
	os.WriteFile(filepath.Join(dir, "big.jpg"), big, 0o644)
	ws := newWS(t, dir)
	read := Read(ws)
	run := func(p string) agenttool.Result {
		t.Helper()
		args, _ := json.Marshal(map[string]string{"path": p})
		res, err := read.Execute(context.Background(), agenttool.Call{ID: "c", Args: args})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := run("shot.png")
	if len(res.Output.Parts) != 2 {
		t.Fatalf("parts %v", res.Output.Parts)
	}
	if img, ok := res.Output.Parts[1].(*openresponses.InputImage); !ok || !strings.HasPrefix(img.ImageURL, "data:image/png;base64,") {
		t.Errorf("image part %#v", res.Output.Parts[1])
	}
	if txt, ok := res.Output.Parts[0].(*openresponses.InputText); !ok || !strings.Contains(txt.Text, "shot.png: image/png") {
		t.Errorf("text part %#v", res.Output.Parts[0])
	}
	if res := run("fake.png"); res.Output.Parts != nil || !strings.Contains(res.Output.Text, "just text") {
		t.Errorf("a .png of text: %+v", res.Output)
	}
	if res := run("big.jpg"); res.Output.Parts != nil || !strings.Contains(res.Output.Text, "read returns an image of at most") {
		t.Errorf("a large image: %+v", res.Output)
	}
}
