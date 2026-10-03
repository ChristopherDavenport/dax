package tool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ChristopherDavenport/agenttool"
)

const (
	defaultReadLines = 2000
	maxReadBytes     = 200 << 10
)

// resolve makes path absolute against dir. It does not sandbox; the
// model may read anywhere the user can.
func resolve(dir, path string) (string, error) {
	if path == "" {
		return "", errors.New("path is required")
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	return filepath.Join(dir, path), nil
}

// ReadArgs are the arguments of the read tool.
type ReadArgs struct {
	Path   string `json:"path" desc:"File path, absolute or relative to the working directory"`
	Offset int    `json:"offset,omitempty" desc:"1-based line to start at (default 1)"`
	Limit  int    `json:"limit,omitempty" desc:"Maximum lines to return (default 2000)"`
}

// Read returns a tool that reads a file with line numbers.
func Read(dir string) agenttool.Tool {
	return agenttool.New("read", "Read a file. Returns numbered lines. Use offset and limit for large files.",
		func(_ context.Context, in ReadArgs) (string, error) {
			p, err := resolve(dir, in.Path)
			if err != nil {
				return "", err
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return "", err
			}
			lines := strings.Split(string(data), "\n")
			if len(lines) > 0 && lines[len(lines)-1] == "" {
				lines = lines[:len(lines)-1]
			}
			start := max(in.Offset, 1)
			if start > len(lines) {
				return fmt.Sprintf("(file has %d lines; offset %d is past the end)", len(lines), start), nil
			}
			limit := in.Limit
			if limit <= 0 {
				limit = defaultReadLines
			}
			end := min(start-1+limit, len(lines))
			var b strings.Builder
			for i := start - 1; i < end; i++ {
				fmt.Fprintf(&b, "%6d\t%s\n", i+1, lines[i])
			}
			if end < len(lines) {
				fmt.Fprintf(&b, "... (%d more lines; use offset=%d)\n", len(lines)-end, end+1)
			}
			return truncate(b.String(), maxReadBytes), nil
		})
}

// WriteArgs are the arguments of the write tool.
type WriteArgs struct {
	Path    string `json:"path" desc:"File path"`
	Content string `json:"content" desc:"Full file content"`
}

// Write returns a tool that creates or replaces a file.
func Write(dir string) agenttool.Tool {
	return agenttool.New("write", "Create or overwrite a file with the given content. Parent directories are created.",
		func(_ context.Context, in WriteArgs) (string, error) {
			p, err := resolve(dir, in.Path)
			if err != nil {
				return "", err
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(p, []byte(in.Content), 0o644); err != nil {
				return "", err
			}
			return fmt.Sprintf("wrote %d bytes to %s", len(in.Content), p), nil
		})
}

// EditArgs are the arguments of the edit tool.
type EditArgs struct {
	Path string `json:"path" desc:"File path"`
	Old  string `json:"old_string" desc:"Exact text to find"`
	New  string `json:"new_string" desc:"Replacement text"`
}

// Edit returns a tool that replaces one exact occurrence of a string.
func Edit(dir string) agenttool.Tool {
	return agenttool.New("edit", "Replace old_string with new_string in a file. old_string must appear exactly once; include enough surrounding lines to make it unique.",
		func(_ context.Context, in EditArgs) (string, error) {
			if in.Old == "" {
				return "", errors.New("old_string must not be empty")
			}
			p, err := resolve(dir, in.Path)
			if err != nil {
				return "", err
			}
			data, err := os.ReadFile(p)
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
			if err := os.WriteFile(p, []byte(out), 0o644); err != nil {
				return "", err
			}
			return fmt.Sprintf("edited %s", p), nil
		})
}
