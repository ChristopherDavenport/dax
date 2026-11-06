package tool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/openresponses"
)

const (
	defaultReadLines = 2000
	maxReadBytes     = 100 << 10
)

// ReadArgs are the arguments of the read tool.
type ReadArgs struct {
	Path   string `json:"path" desc:"File path, relative to the workspace or absolute inside it"`
	Offset int    `json:"offset,omitempty" desc:"1-based line to start at (default 1)"`
	Limit  int    `json:"limit,omitempty" desc:"Maximum lines to return (default 2000)"`
	// Stamp is the stamp of the facts the policy decided this call on
	// (dax sets it); the tool runs only if they still hold.
	Stamp string `json:"dax_stamp,omitempty" desc:"Set by dax; leave it out"`
}

// DefaultMaxRead is the most bytes of one file read scans, and the
// largest file edit will rewrite.
const DefaultMaxRead = 2 << 20

// maxLineBytes bounds one line of read's output; the rest of a longer
// line is dropped, so a file of one huge line costs no more than this.
const maxLineBytes = 64 << 10

// ReadOption configures the read tool.
type ReadOption func(*readConfig)

type readConfig struct{ max int64 }

// WithMaxRead sets the most bytes one read call scans, which is also
// the largest file edit will rewrite. Zero or less is DefaultMaxRead.
func WithMaxRead(n int64) ReadOption { return func(c *readConfig) { c.max = n } }

func readCap(opts []ReadOption) int64 {
	c := readConfig{max: DefaultMaxRead}
	for _, o := range opts {
		o(&c)
	}
	if c.max <= 0 {
		c.max = DefaultMaxRead
	}
	return c.max
}

// Read returns a tool that reads a file with line numbers. It streams
// the file: memory is bounded by the output and one line, not by the
// file, and it scans at most the size cap, so a multi-gigabyte file
// costs what a small one does. Past the cap, use grep to find the
// line.
func Read(ws *Files, opts ...ReadOption) agenttool.Tool {
	return exactArgs[ReadArgs](readTool(ws, agenttool.WithFacts(pathFacts(ws, "read", "path", "")), opts...))
}

func readTool(ws *Files, claim agenttool.Option, opts ...ReadOption) agenttool.Tool {
	limitBytes := readCap(opts)
	return agenttool.New("read", "Read a file. Returns numbered lines. Use offset and limit for large files; only the first "+fmt.Sprint(limitBytes>>10)+" KiB of a file can be read, so use grep to find a line in a larger one. An image (PNG, JPEG, GIF or WebP, up to "+fmt.Sprint(MaxImageBytes>>20)+" MiB) is returned as the image, for a model that takes images.",
		func(_ context.Context, in ReadArgs) (agenttool.Result, error) {
			text := func(s string, err error) (agenttool.Result, error) {
				return agenttool.Result{Output: openresponses.FunctionCallOutputData{Text: s}}, err
			}
			if err := ws.checkTouched("read", "", in.Path, in.Stamp); err != nil {
				return text("", err)
			}
			if mt := ImageType(in.Path, nil); mt != "" {
				f, _, size, err := ws.openRegular(in.Path)
				if err != nil {
					return text("", err)
				}
				res, ok, err := readImage(f, in.Path, mt, size)
				f.Close()
				if ok || err != nil {
					return res, err
				}
				// Not the image its name says: read as text.
			}
			f, _, size, err := ws.openRegular(in.Path)
			if err != nil {
				return text("", err)
			}
			defer f.Close()
			start := max(in.Offset, 1)
			limit := in.Limit
			if limit <= 0 {
				limit = defaultReadLines
			}
			br := bufio.NewReaderSize(io.LimitReader(f, limitBytes), 64<<10)
			var b strings.Builder
			n, shown := 0, 0 // lines seen, lines shown
			full := false
			for {
				line, ok := readLine(br)
				if !ok {
					break
				}
				n++
				if n < start {
					continue
				}
				if shown >= limit || b.Len() >= maxReadBytes {
					full = true
					continue // count the rest
				}
				fmt.Fprintf(&b, "%6d\t%s\n", n, line)
				shown++
			}
			capped := size > limitBytes
			switch {
			case shown == 0 && !capped:
				return text(fmt.Sprintf("(file has %d lines; offset %d is past the end)", n, start), nil)
			case shown == 0:
				return text(fmt.Sprintf("(offset %d is beyond the first %d bytes of this %d-byte file, which is all read will scan; use grep to find a line)", start, limitBytes, size), nil)
			}
			if full {
				fmt.Fprintf(&b, "... (%d more lines; use offset=%d)\n", n-(start-1)-shown, start+shown)
			}
			if capped {
				fmt.Fprintf(&b, "... (stopped after the first %d of %d bytes; use grep to find a line later in the file)\n", limitBytes, size)
			}
			return text(truncate(b.String(), maxReadBytes+maxLineBytes), nil)
		}, claim)
}

// MaxImageBytes is the largest image read returns as one: about what a
// provider takes in one image.
const MaxImageBytes = 5 << 20

// imageMagic are the image types read returns as images, by extension,
// each with the bytes its data starts with.
var imageMagic = map[string]struct {
	mediaType string
	magic     func([]byte) bool
}{
	".png":  {"image/png", func(b []byte) bool { return bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")) }},
	".jpg":  {"image/jpeg", func(b []byte) bool { return bytes.HasPrefix(b, []byte{0xff, 0xd8, 0xff}) }},
	".jpeg": {"image/jpeg", func(b []byte) bool { return bytes.HasPrefix(b, []byte{0xff, 0xd8, 0xff}) }},
	".gif": {"image/gif", func(b []byte) bool {
		return bytes.HasPrefix(b, []byte("GIF87a")) || bytes.HasPrefix(b, []byte("GIF89a"))
	}},
	".webp": {"image/webp", func(b []byte) bool { return len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP" }},
}

// ImageType is the media type of an image file named name, by its
// extension, any case; with data, only when data starts as that type
// does. "" for a name that is no image read returns, or data that is
// not the image its name says.
func ImageType(name string, data []byte) string {
	t, ok := imageMagic[strings.ToLower(path.Ext(name))]
	if !ok || data != nil && !t.magic(data) {
		return ""
	}
	return t.mediaType
}

// ImageURL is data as a data URL of media type mt.
func ImageURL(mt string, data []byte) string {
	return "data:" + mt + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// readImage is read's result for the image file f, named name, of size
// bytes and media type mt by its name: the image, with a line saying
// what it is, as an image part. ok is false when its bytes are not of
// that type, to be read as text. One over MaxImageBytes is said, not
// sent.
func readImage(f io.Reader, name, mt string, size int64) (agenttool.Result, bool, error) {
	if size > MaxImageBytes {
		return agenttool.Result{Output: openresponses.FunctionCallOutputData{Text: fmt.Sprintf("(%s is a %d-byte image; read returns an image of at most %d bytes)", name, size, MaxImageBytes)}}, true, nil
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxImageBytes+1))
	if err != nil {
		return agenttool.Result{}, false, err
	}
	if len(data) > MaxImageBytes {
		return agenttool.Result{Output: openresponses.FunctionCallOutputData{Text: fmt.Sprintf("(%s is an image of more than %d bytes, which is all read returns)", name, MaxImageBytes)}}, true, nil
	}
	if ImageType(name, data) == "" {
		return agenttool.Result{}, false, nil
	}
	return agenttool.Result{Output: openresponses.FunctionCallOutputData{Parts: openresponses.Contents{
		&openresponses.InputText{Text: fmt.Sprintf("%s: %s, %d bytes", name, mt, len(data))},
		&openresponses.InputImage{ImageURL: ImageURL(mt, data)},
	}}}, true, nil
}

// readLine reads one line without its newline, keeping at most
// maxLineBytes of it; ok is false at the end of the input.
func readLine(br *bufio.Reader) (string, bool) {
	var line []byte
	got := false
	for {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 {
			got = true
			if room := maxLineBytes - len(line); room > 0 {
				line = append(line, chunk[:min(room, len(chunk))]...)
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if !got {
			return "", false
		}
		s := strings.TrimRight(string(line), "\r\n")
		if len(line) >= maxLineBytes {
			s += "... [line truncated]"
		}
		return s, true
	}
}

// WriteArgs are the arguments of the write tool.
type WriteArgs struct {
	Path    string `json:"path" desc:"File path"`
	Content string `json:"content" desc:"Full file content"`
	// Stamp is the stamp of the facts the policy decided this call on
	// (dax sets it); the tool runs only if they still hold.
	Stamp string `json:"dax_stamp,omitempty" desc:"Set by dax; leave it out"`
}

// Write returns a tool that creates or replaces a file.
func Write(ws *Files) agenttool.Tool {
	return exactArgs[WriteArgs](writeTool(ws, agenttool.WithFacts(pathFacts(ws, "write", "path", ""))))
}

func writeTool(ws *Files, claim agenttool.Option) agenttool.Tool {
	return agenttool.New("write", "Create or overwrite a file with the given content. Parent directories are created.",
		func(_ context.Context, in WriteArgs) (string, error) {
			if err := ws.checkTouched("write", "", in.Path, in.Stamp); err != nil {
				return "", err
			}
			ws.writes.Lock()
			defer ws.writes.Unlock()
			rel, err := ws.writeFile(in.Path, []byte(in.Content), true)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("wrote %d bytes to %s", len(in.Content), rel), nil
		}, claim)
}

// EditArgs are the arguments of the edit tool.
type EditArgs struct {
	Path string `json:"path" desc:"File path"`
	Old  string `json:"old_string" desc:"Exact text to find"`
	New  string `json:"new_string" desc:"Replacement text"`
	// Stamp is the stamp of the facts the policy decided this call on
	// (dax sets it); the tool runs only if they still hold.
	Stamp string `json:"dax_stamp,omitempty" desc:"Set by dax; leave it out"`
}

// Edit returns a tool that replaces one exact occurrence of a string.
func Edit(ws *Files, opts ...ReadOption) agenttool.Tool {
	return exactArgs[EditArgs](editTool(ws, agenttool.WithFacts(pathFacts(ws, "edit", "path", "")), opts...))
}

func editTool(ws *Files, claim agenttool.Option, opts ...ReadOption) agenttool.Tool {
	limitBytes := readCap(opts)
	return agenttool.New("edit", "Replace old_string with new_string in a file. old_string must appear exactly once; include enough surrounding lines to make it unique.",
		func(_ context.Context, in EditArgs) (string, error) {
			if err := ws.checkTouched("edit", "", in.Path, in.Stamp); err != nil {
				return "", err
			}
			if in.Old == "" {
				return "", errors.New("old_string must not be empty")
			}
			ws.writes.Lock()
			defer ws.writes.Unlock()
			data, err := ws.readFileMax(in.Path, limitBytes)
			if err != nil {
				return "", err
			}
			src := string(data)
			switch n := strings.Count(src, in.Old); n {
			case 0:
				return "", errors.New("old_string not found in file")
			case 1:
			default:
				return "", fmt.Errorf("old_string matches %d times; add context to make it unique", n)
			}
			out := strings.Replace(src, in.Old, in.New, 1)
			rel, err := ws.writeFile(in.Path, []byte(out), false)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("edited %s", rel), nil
		}, claim)
}
