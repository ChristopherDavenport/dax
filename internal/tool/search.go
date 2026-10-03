package tool

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/ChristopherDavenport/agenttool"
)

const (
	defaultGlobResults = 1000
	defaultGrepResults = 200
	maxGrepFileBytes   = 2 << 20
	maxGrepLineBytes   = 500
)

// skipDirs are directories glob and grep do not descend into: version
// control metadata and the usual vendored or generated trees. There is
// no .gitignore reader; naming one of these as the search path, or
// reading it with ls, still works.
var skipDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true,
	"node_modules": true, "vendor": true,
	".venv": true, "venv": true, "__pycache__": true,
	".cache": true, ".idea": true, ".next": true,
}

// walk visits every file under base (a name relative to the root, "."
// for the root itself) in name order, skipping skipDirs below base. A
// symbolic link to a directory is not followed, and a link whose
// target is missing or outside the workspace is skipped. If base names
// a file it is the only visit.
func (w *Workspace) walk(ctx context.Context, base string, visit func(rel string) (stop bool, err error)) error {
	info, err := w.stat(base)
	if err != nil {
		return wrap(base, err)
	}
	if !info.IsDir() {
		_, err := visit(base)
		return err
	}
	var rec func(dir string) (bool, error)
	rec = func(dir string) (bool, error) {
		if err := ctx.Err(); err != nil {
			return true, err
		}
		entries, err := w.readDir(dir)
		if err != nil {
			return false, nil // an unreadable directory is skipped
		}
		slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
		for _, e := range entries {
			rel := path.Join(filepath.ToSlash(dir), e.Name())
			if dir == "." {
				rel = e.Name()
			}
			typ := e.Type()
			if typ&fs.ModeSymlink != 0 {
				fi, err := w.stat(rel)
				if err != nil || fi.IsDir() {
					continue
				}
				typ = 0
			}
			switch {
			case typ.IsDir():
				if skipDirs[e.Name()] {
					continue
				}
				if stop, err := rec(rel); stop || err != nil {
					return stop, err
				}
			case typ.IsRegular():
				if stop, err := visit(rel); stop || err != nil {
					return stop, err
				}
			}
		}
		return false, nil
	}
	_, err = rec(filepath.ToSlash(base))
	return err
}

// relBase resolves the optional path argument of a search to a name
// relative to the root.
func (w *Workspace) relBase(p string) (string, error) {
	if p == "" || p == "." {
		return ".", nil
	}
	return w.Rel(p)
}

// ---- glob

// GlobArgs are the arguments of the glob tool.
type GlobArgs struct {
	Pattern    string `json:"pattern" desc:"Glob pattern, e.g. **/*.go or cmd/*/main.go. ** matches any number of directories; * and ? stay within one path segment; {a,b} alternates"`
	Path       string `json:"path,omitempty" desc:"Directory to search, relative to the workspace (default the workspace root); the pattern is relative to it"`
	MaxResults int    `json:"max_results,omitempty" desc:"Maximum paths to return (default 1000)"`
}

// Glob returns a tool that lists files whose path matches a
// doublestar-style pattern, sorted. It skips .git and common vendored
// directories.
func Glob(ws *Workspace) agenttool.Tool {
	return agenttool.New("glob", "Find files by glob pattern, e.g. **/*.go. Paths are relative to the search directory and sorted. Skips .git, node_modules, vendor and similar directories.",
		func(ctx context.Context, in GlobArgs) (string, error) {
			if in.Pattern == "" {
				return "", errors.New("pattern is required")
			}
			pats, err := expandBraces(in.Pattern)
			if err != nil {
				return "", err
			}
			for _, p := range pats {
				if _, err := path.Match(strings.ReplaceAll(p, "**", "*"), ""); err != nil {
					return "", fmt.Errorf("bad pattern %q: %w", in.Pattern, err)
				}
			}
			base, err := ws.relBase(in.Path)
			if err != nil {
				return "", err
			}
			limit := in.MaxResults
			if limit <= 0 {
				limit = defaultGlobResults
			}
			var hits []string
			truncated := false
			err = ws.walk(ctx, base, func(rel string) (bool, error) {
				sub := rel
				if base != "." {
					sub, _ = filepath.Rel(base, filepath.FromSlash(rel))
					sub = filepath.ToSlash(sub)
				}
				for _, p := range pats {
					if matchGlob(p, sub) {
						if len(hits) >= limit {
							truncated = true
							return true, nil
						}
						hits = append(hits, rel)
						break
					}
				}
				return false, nil
			})
			if err != nil {
				return "", err
			}
			sort.Strings(hits)
			if len(hits) == 0 {
				return "(no files match)", nil
			}
			out := strings.Join(hits, "\n")
			if truncated {
				out += fmt.Sprintf("\n... (stopped at %d results; narrow the pattern or path)", limit)
			}
			return out, nil
		})
}

// matchGlob matches a slash-separated name against a pattern whose
// segments are path.Match patterns, with a segment of ** standing for
// any number of segments, none included.
func matchGlob(pattern, name string) bool {
	return matchSegs(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegs(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for len(pat) > 1 && pat[1] == "**" {
				pat = pat[1:]
			}
			if len(pat) == 1 {
				return true
			}
			for i := 0; i <= len(name); i++ {
				if matchSegs(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], name[0]); err != nil || !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// expandBraces expands {a,b} alternations, which path.Match lacks.
func expandBraces(p string) ([]string, error) {
	open := strings.IndexByte(p, '{')
	if open < 0 {
		return []string{p}, nil
	}
	depth, closeAt := 0, -1
	var cuts []int
	for i := open; i < len(p) && closeAt < 0; i++ {
		switch p[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				closeAt = i
			}
		case ',':
			if depth == 1 {
				cuts = append(cuts, i)
			}
		}
	}
	if closeAt < 0 {
		return nil, fmt.Errorf("bad pattern %q: unclosed {", p)
	}
	var out []string
	start := open + 1
	for _, end := range append(cuts, closeAt) {
		rest, err := expandBraces(p[:open] + p[start:end] + p[closeAt+1:])
		if err != nil {
			return nil, err
		}
		out = append(out, rest...)
		start = end + 1
	}
	return out, nil
}

// ---- grep

// GrepArgs are the arguments of the grep tool.
type GrepArgs struct {
	Pattern    string `json:"pattern" desc:"Regular expression (Go RE2 syntax)"`
	Path       string `json:"path,omitempty" desc:"File or directory to search, relative to the workspace (default the workspace root)"`
	Include    string `json:"include,omitempty" desc:"Only search files whose path matches this glob, e.g. *.go or internal/**/*.go; a pattern with no / matches the file name"`
	IgnoreCase bool   `json:"ignore_case,omitempty" desc:"Match case-insensitively"`
	MaxResults int    `json:"max_results,omitempty" desc:"Maximum matching lines to return (default 200)"`
}

// Grep returns a tool that searches files for a regular expression and
// returns path:line:text for each matching line.
func Grep(ws *Workspace) agenttool.Tool {
	return agenttool.New("grep", "Search file contents for a regular expression. Returns path:line:text, sorted by path. Skips binary files, .git, node_modules, vendor and similar directories.",
		func(ctx context.Context, in GrepArgs) (string, error) {
			if in.Pattern == "" {
				return "", errors.New("pattern is required")
			}
			expr := in.Pattern
			if in.IgnoreCase {
				expr = "(?i)" + expr
			}
			re, err := regexp.Compile(expr)
			if err != nil {
				return "", fmt.Errorf("bad pattern: %w", err)
			}
			var includes []string
			if in.Include != "" {
				if includes, err = expandBraces(in.Include); err != nil {
					return "", err
				}
			}
			base, err := ws.relBase(in.Path)
			if err != nil {
				return "", err
			}
			limit := in.MaxResults
			if limit <= 0 {
				limit = defaultGrepResults
			}
			var b strings.Builder
			n, truncated := 0, false
			err = ws.walk(ctx, base, func(rel string) (bool, error) {
				if includes != nil && !includeMatch(includes, rel) {
					return false, nil
				}
				stop := ws.grepFile(rel, re, func(line int, text string) bool {
					if n >= limit {
						truncated = true
						return false
					}
					if len(text) > maxGrepLineBytes {
						text = text[:maxGrepLineBytes] + "..."
					}
					fmt.Fprintf(&b, "%s:%d:%s\n", rel, line, text)
					n++
					return true
				})
				return stop, nil
			})
			if err != nil {
				return "", err
			}
			if n == 0 {
				return "(no matches)", nil
			}
			out := strings.TrimSuffix(b.String(), "\n")
			if truncated {
				out += fmt.Sprintf("\n... (stopped at %d matches; narrow the pattern or path)", limit)
			}
			return out, nil
		})
}

func includeMatch(globs []string, rel string) bool {
	for _, g := range globs {
		if !strings.Contains(g, "/") {
			if ok, _ := path.Match(g, path.Base(rel)); ok {
				return true
			}
			continue
		}
		if matchGlob(g, rel) {
			return true
		}
	}
	return false
}

// grepFile reports each matching line to emit until it returns false
// and says whether the search is over.
func (w *Workspace) grepFile(rel string, re *regexp.Regexp, emit func(line int, text string) bool) (stop bool) {
	f, err := w.open(rel)
	if err != nil {
		return false
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || fi.Size() > maxGrepFileBytes {
		return false
	}
	br := bufio.NewReaderSize(f, 64<<10)
	head, _ := br.Peek(8000)
	if bytes.IndexByte(head, 0) >= 0 {
		return false
	}
	for line := 1; ; line++ {
		text, err := br.ReadString('\n')
		if text != "" {
			text = strings.TrimRight(text, "\r\n")
			if re.MatchString(text) && !emit(line, text) {
				return true
			}
		}
		if err != nil {
			if err != io.EOF {
				return false
			}
			return false
		}
	}
}

// ---- ls

// LSArgs are the arguments of the ls tool.
type LSArgs struct {
	Path string `json:"path,omitempty" desc:"Directory to list, relative to the workspace (default the workspace root)"`
}

// LS returns a tool that lists one directory: names sorted, directories
// with a trailing slash, files with their size.
func LS(ws *Workspace) agenttool.Tool {
	return agenttool.New("ls", "List the entries of one directory, sorted: directories end in /, files show their size in bytes.",
		func(_ context.Context, in LSArgs) (string, error) {
			rel, err := ws.relBase(in.Path)
			if err != nil {
				return "", err
			}
			info, err := ws.stat(rel)
			if err != nil {
				return "", wrap(in.Path, err)
			}
			if !info.IsDir() {
				return "", fmt.Errorf("%s is not a directory", rel)
			}
			entries, err := ws.readDir(rel)
			if err != nil {
				return "", wrap(in.Path, err)
			}
			slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
			if len(entries) == 0 {
				return "(empty)", nil
			}
			var b strings.Builder
			for _, e := range entries {
				name := e.Name()
				switch {
				case e.IsDir():
					b.WriteString(name + "/\n")
				case e.Type()&fs.ModeSymlink != 0:
					fi, err := ws.stat(path.Join(filepath.ToSlash(rel), name))
					switch {
					case err != nil:
						b.WriteString(name + "@ (link outside the workspace or broken)\n")
					case fi.IsDir():
						b.WriteString(name + "@/\n")
					default:
						fmt.Fprintf(&b, "%s@  %d\n", name, fi.Size())
					}
				default:
					size := int64(0)
					if fi, err := e.Info(); err == nil {
						size = fi.Size()
					}
					fmt.Fprintf(&b, "%s  %d\n", name, size)
				}
			}
			return strings.TrimSuffix(b.String(), "\n"), nil
		})
}
