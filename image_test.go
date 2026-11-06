package dax

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
)

// fakeImager is controls that attach images.
type fakeImager struct {
	fakeControls
	asked []string
}

func (f *fakeImager) Image(_ context.Context, path string) (*openresponses.InputImage, error) {
	f.asked = append(f.asked, path)
	if strings.Contains(path, "bad") {
		return nil, errors.New("not \x1b[2Jan image")
	}
	return &openresponses.InputImage{ImageURL: "data:image/png;base64,AA=="}, nil
}

// #166: @path.png in a typed line attaches the image to the person's
// message, once however often it is named; a word that is no image
// name is text; an image that cannot be attached sends nothing, said
// cleaned. Over dax attach the line goes as typed.
func TestATypedLineAttachesItsImages(t *testing.T) {
	ctx := context.Background()
	f := &fakeImager{}
	items, err := typedItems(ctx, f, "fix this @/tmp/form.png, see @/tmp/form.png. and @notes.txt or me@x.png")
	if err != nil {
		t.Fatal(err)
	}
	msg, ok := items[0].(*openresponses.Message)
	if !ok || len(items) != 1 || len(msg.Content) != 2 || msg.Role != openresponses.RoleUser {
		t.Fatalf("items %#v", items)
	}
	if _, ok := msg.Content[1].(*openresponses.InputImage); !ok {
		t.Errorf("content %#v", msg.Content)
	}
	if len(f.asked) != 1 || f.asked[0] != "/tmp/form.png" {
		t.Errorf("asked for %q", f.asked)
	}
	if _, err := typedItems(ctx, f, "@bad.png"); err == nil || strings.Contains(err.Error(), "\x1b") {
		t.Errorf("a bad image: %q", err)
	}
	if items, _ := typedItems(ctx, &fakeControls{}, "@/tmp/form.png"); len(items[0].(*openresponses.Message).Content) != 1 {
		t.Error("over attach an image was attached")
	}
}

// fakeBoth is controls that attach images and whose server "team"
// offers resources.
type fakeBoth struct {
	fakeMentions
	asked []string
}

func (f *fakeBoth) Image(_ context.Context, path string) (*openresponses.InputImage, error) {
	f.asked = append(f.asked, path)
	return &openresponses.InputImage{ImageURL: "data:image/png;base64,AA=="}, nil
}

func (f *fakeBoth) IsMCPMention(word string) bool { return strings.HasPrefix(word, "@team:") }

// A mention of a server's resource whose URI ends as an image's name
// does is the server's, never a path: @team:shot.png attaches the
// resource and reads no file; a name no server has stays a path.
func TestAnMCPMentionWinsOverAnImagePath(t *testing.T) {
	ctx := context.Background()
	f := &fakeBoth{}
	items, err := typedItems(ctx, f, "see @team:shot.png and @other:x.png")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Errorf("%d items, want the line and the resource", len(items))
	}
	if len(f.asked) != 1 || f.asked[0] != "other:x.png" {
		t.Errorf("images asked for: %q, want only other:x.png", f.asked)
	}
}
