package agent

// Images: a file read returns one (dax-coding's read), and the person
// may attach one of the workspace's files to a line (@path.png); either
// reaches the model only when its vendor says it takes images
// (Options.TakesImages). The
// session's store keeps them as blobs beside the log (agentsession's
// sidecar media, header media "sidecar"), so the log names each by its
// hash rather than holding it inline (#166).

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn/compact"
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
		res.Output.Parts = sayImages(res.Output.Parts, s.Model())
		return res, nil
	})
}

// textOnly is inner with the images dax puts in a request, a read's
// and the person's own, said in place of shown when the request's model
// does not take them: judged at each request by the model it names, so
// a sub-agent on a model of its own is judged by that model, and the
// images a conversation already holds are not sent to a text-only model
// after /model, a resume or /resume (#166). Any other tool's images (an
// MCP server's) pass as they did before. A server's compaction is kept,
// its input judged the same way.
func textOnly(takes func(context.Context, string) bool, inner openresponses.Streamer) openresponses.Streamer {
	if inner == nil {
		return nil
	}
	t := imageless{inner: inner, takes: takes}
	if c, ok := inner.(compact.Compactor); ok {
		return imagelessCompactor{imageless: t, c: c}
	}
	return t
}

type imageless struct {
	inner openresponses.Streamer
	takes func(context.Context, string) bool
}

func (s imageless) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	req.Input = s.strip(ctx, req.Model, req.Input)
	return s.inner.CreateStream(ctx, req, sink)
}

type imagelessCompactor struct {
	imageless
	c compact.Compactor
}

func (s imagelessCompactor) Compact(ctx context.Context, req openresponses.CompactRequest) (*openresponses.CompactResponse, error) {
	req.Input = s.strip(ctx, req.Model, req.Input)
	return s.c.Compact(ctx, req)
}

// strip is in with the person's images and read's said, not shown, when
// model does not take images; in itself when it holds none or the model
// takes them. The items changed are copies: in is the conversation's.
func (s imageless) strip(ctx context.Context, model string, in openresponses.Items) openresponses.Items {
	reads := map[string]bool{}
	var found bool
	for _, it := range in {
		switch it := it.(type) {
		case *openresponses.FunctionCall:
			if it.Name == "read" {
				reads[it.CallID] = true
			}
		case *openresponses.Message:
			found = found || it.Role == openresponses.RoleUser && hasImage(it.Content)
		case *openresponses.FunctionCallOutput:
			found = found || hasImage(it.Output.Parts)
		}
	}
	if !found || s.takes != nil && s.takes(ctx, model) {
		return in
	}
	out := make(openresponses.Items, len(in))
	for i, it := range in {
		out[i] = it
		switch it := it.(type) {
		case *openresponses.Message:
			if it.Role == openresponses.RoleUser && hasImage(it.Content) {
				m := *it
				m.Content = sayImages(it.Content, model)
				out[i] = &m
			}
		case *openresponses.FunctionCallOutput:
			if reads[it.CallID] && hasImage(it.Output.Parts) {
				o := *it
				o.Output.Parts = sayImages(it.Output.Parts, model)
				out[i] = &o
			}
		}
	}
	return out
}

func hasImage(cs openresponses.Contents) bool {
	for _, c := range cs {
		if _, ok := c.(*openresponses.InputImage); ok {
			return true
		}
	}
	return false
}

// sayImages is cs with each image a line saying model is not shown it.
func sayImages(cs openresponses.Contents, model string) openresponses.Contents {
	out := make(openresponses.Contents, len(cs))
	for i, c := range cs {
		out[i] = c
		if _, ok := c.(*openresponses.InputImage); ok {
			out[i] = &openresponses.InputText{Text: noImage(model)}
		}
	}
	return out
}

// Image is the image file at path in the session's workspace as a part
// of the person's message (@path.png in a line they type): the
// project's file, read through the workspace as read reads one, so a
// path is relative to the workspace's root and one that leaves it, by
// name or by a link, is tool.ErrOutside, whatever the workspace (this
// machine's directory, a box, an executor, a served one); a PNG, JPEG,
// GIF or WebP of at most tool.MaxImageBytes, by its name and its bytes.
// It is refused for a model whose vendor does not say it takes images.
func (s *Session) Image(ctx context.Context, path string) (*openresponses.InputImage, error) {
	if !s.takesImages(ctx) {
		return nil, errors.New(strings.Trim(noImage(s.Model()), "[]"))
	}
	if strings.HasPrefix(path, "~") {
		return nil, fmt.Errorf("%w: %s (an attached image is a file in the workspace)", tool.ErrOutside, path)
	}
	mt := tool.ImageType(path, nil)
	if mt == "" {
		return nil, fmt.Errorf("%s: an image is a PNG, JPEG, GIF or WebP file", path)
	}
	data, err := s.files.ReadFile(path, tool.MaxImageBytes)
	if err != nil {
		return nil, err
	}
	if tool.ImageType(path, data) == "" {
		return nil, fmt.Errorf("%s is not the %s its name says", path, mt)
	}
	return &openresponses.InputImage{ImageURL: tool.ImageURL(mt, data)}, nil
}
