package agent

// Images: a file read returns one (dax-coding's read), and the person
// may attach one to a line (@path.png); either reaches the model only
// when its vendor says it takes images (Options.TakesImages). The
// session's store keeps them as blobs beside the log (agentsession's
// sidecar media, header media "sidecar"), so the log names each by its
// hash rather than holding it inline (#166).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/tool"
)

// takesImages reports whether the model in force takes images, as its
// vendor says.
func (s *Session) takesImages(ctx context.Context) bool {
	return s.opts.TakesImages != nil && s.opts.TakesImages(ctx, s.Model())
}

// noImage is what the model is told in place of an image it cannot be
// shown.
func noImage(model string) string {
	return fmt.Sprintf("[an image is not shown: model %s does not take images, as its vendor says]", model)
}

// imageGate is read with the images it returns shown to a model that
// takes them and said to one that does not: the tool runs wherever the
// tools act and cannot know the model, so the session decides. Any
// other tool is t.
func (s *Session) imageGate(t agenttool.Tool) agenttool.Tool {
	if t == nil || t.Name() != "read" {
		return t
	}
	return agenttool.Wrap(t, func(ctx context.Context, c agenttool.Call) (agenttool.Result, error) {
		res, err := t.Execute(ctx, c)
		if err != nil || res.Output.Parts == nil || s.takesImages(ctx) {
			return res, err
		}
		parts := make(openresponses.Contents, 0, len(res.Output.Parts))
		for _, p := range res.Output.Parts {
			if _, ok := p.(*openresponses.InputImage); ok {
				p = &openresponses.InputText{Text: noImage(s.Model())}
			}
			parts = append(parts, p)
		}
		res.Output.Parts = parts
		return res, nil
	})
}

// Image is the image file at path on this machine as a part of the
// person's message (@path.png in a line they type): read here, since it
// is theirs to attach, as running a command is, never the model's; a
// PNG, JPEG, GIF or WebP of at most tool.MaxImageBytes, by its name and
// its bytes. A leading ~/ is the home directory. It is refused for a
// model whose vendor does not say it takes images.
func (s *Session) Image(ctx context.Context, path string) (*openresponses.InputImage, error) {
	if !s.takesImages(ctx) {
		return nil, errors.New(strings.Trim(noImage(s.Model()), "[]"))
	}
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(home, rest)
	}
	mt := tool.ImageType(path, nil)
	if mt == "" {
		return nil, fmt.Errorf("%s: an image is a PNG, JPEG, GIF or WebP file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil {
		return nil, err
	} else if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, tool.MaxImageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > tool.MaxImageBytes {
		return nil, fmt.Errorf("%s is over the %d bytes an image may be", path, tool.MaxImageBytes)
	}
	if tool.ImageType(path, data) == "" {
		return nil, fmt.Errorf("%s is not the %s its name says", path, mt)
	}
	return &openresponses.InputImage{ImageURL: tool.ImageURL(mt, data)}, nil
}
