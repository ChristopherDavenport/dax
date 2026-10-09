package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	workspace "github.com/ChristopherDavenport/agentworkspace"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// filesBox is a server whose workspace holds a file, a directory, a
// link inside, a link out to a secret, a link to nothing, a file over
// the read bound and, where it can be made, a FIFO.
func filesBox(t *testing.T) (executorServer, string) {
	t.Helper()
	s := newServer(t)
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret"), "SECRET=1\n")
	writeFile(t, filepath.Join(s.dir, "AGENTS.md"), "Be brief.\n")
	writeFile(t, filepath.Join(s.dir, "dir", "a.txt"), "a\n")
	writeFile(t, filepath.Join(s.dir, "dir", "sub", "b.txt"), "b\n")
	writeFile(t, filepath.Join(s.dir, "big"), strings.Repeat("x", MaxFileBytes+1))
	for link, target := range map[string]string{
		"in":      "AGENTS.md",
		"out":     filepath.Join(outside, "secret"),
		"climb":   "../" + filepath.Base(outside) + "/secret",
		"nowhere": "missing",
	} {
		if err := os.Symlink(target, filepath.Join(s.dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	syscall.Mkfifo(filepath.Join(s.dir, "fifo"), 0o644)
	return s, outside
}

// The client reads the executor's workspace through its file system:
// stat follows a link that stays inside, lstat and readlink show it, a
// listing is sorted and typed, and every way out (a link, "..", an
// absolute name) is workspace.ErrOutside, as it is in the executor; an
// invalid name is fs.ErrInvalid and a missing one fs.ErrNotExist. A
// read over MaxFileBytes, of a FIFO or of a directory is refused.
func TestTheClientReadsTheExecutorsFiles(t *testing.T) {
	s, _ := filesBox(t)
	r, _ := s.remote(t)
	fsys := r.FS()
	for _, tc := range []struct {
		name    string
		do      func() (string, error)
		want    string // the result, when no error
		wantErr error  // errors.Is; nil with errText: any error
		errText string
	}{
		{"read a file", func() (string, error) { b, err := fs.ReadFile(fsys, "AGENTS.md"); return string(b), err }, "Be brief.\n", nil, ""},
		{"read through a link inside", func() (string, error) { b, err := fs.ReadFile(fsys, "in"); return string(b), err }, "Be brief.\n", nil, ""},
		{"open and read", func() (string, error) {
			f, err := fsys.Open("dir/a.txt")
			if err != nil {
				return "", err
			}
			defer f.Close()
			b := make([]byte, 10)
			n, err := f.Read(b)
			return string(b[:n]), err
		}, "a\n", nil, ""},
		{"stat follows a link inside", func() (string, error) {
			fi, err := fs.Stat(fsys, "in")
			if err != nil {
				return "", err
			}
			return fi.Name() + " " + fi.Mode().Type().String(), nil
		}, "in ----------", nil, ""},
		{"lstat shows the link", func() (string, error) {
			fi, err := fs.Lstat(fsys, "out")
			if err != nil {
				return "", err
			}
			return fi.Mode().Type().String(), nil
		}, "L---------", nil, ""},
		{"readlink", func() (string, error) { return fs.ReadLink(fsys, "in") }, "AGENTS.md", nil, ""},
		{"a listing, sorted and typed", func() (string, error) {
			es, err := fs.ReadDir(fsys, ".")
			var out []string
			for _, e := range es {
				out = append(out, e.Name()+":"+e.Type().String())
			}
			return strings.Join(out, " "), err
		}, "", nil, ""}, // checked below
		{"stat a link out", func() (string, error) { _, err := fs.Stat(fsys, "out"); return "", err }, "", workspace.ErrOutside, ""},
		{"read a link out", func() (string, error) { _, err := fs.ReadFile(fsys, "out"); return "", err }, "", workspace.ErrOutside, ""},
		{"read a relative link that climbs out", func() (string, error) { _, err := fs.ReadFile(fsys, "climb"); return "", err }, "", workspace.ErrOutside, ""},
		{"open a link out", func() (string, error) { _, err := fsys.Open("out"); return "", err }, "", workspace.ErrOutside, ""},
		{"..", func() (string, error) { _, err := fs.ReadFile(fsys, "../secret"); return "", err }, "", workspace.ErrOutside, ""},
		{"an absolute name", func() (string, error) { _, err := fs.Stat(fsys, "/etc/passwd"); return "", err }, "", workspace.ErrOutside, ""},
		{"an invalid name", func() (string, error) { _, err := fs.Stat(fsys, "dir/../AGENTS.md"); return "", err }, "", fs.ErrInvalid, ""},
		{"a missing name", func() (string, error) { _, err := fs.Stat(fsys, "nothing"); return "", err }, "", fs.ErrNotExist, ""},
		{"a link to nothing", func() (string, error) { _, err := fs.Stat(fsys, "nowhere"); return "", err }, "", fs.ErrNotExist, ""},
		{"readlink of a file", func() (string, error) { return fs.ReadLink(fsys, "AGENTS.md") }, "", nil, "invalid argument"},
		{"a read over the bound", func() (string, error) { _, err := fs.ReadFile(fsys, "big"); return "", err }, "", nil, "over the 1048576-byte limit"},
		{"a read of a directory", func() (string, error) { _, err := fs.ReadFile(fsys, "dir"); return "", err }, "", nil, "not a regular file"},
		{"a read of an opened directory", func() (string, error) {
			f, err := fsys.Open("dir")
			if err != nil {
				return "", err
			}
			defer f.Close()
			_, err = f.Read(make([]byte, 1))
			return "", err
		}, "", nil, "is a directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.do()
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("err = %v, want %v", err, tc.wantErr)
				}
			case tc.errText != "":
				if err == nil || !strings.Contains(err.Error(), tc.errText) {
					t.Errorf("err = %v, want %q", err, tc.errText)
				}
			case err != nil:
				t.Fatal(err)
			case tc.want != "" && got != tc.want:
				t.Errorf("got %q, want %q", got, tc.want)
			case tc.want == "":
				for _, w := range []string{"AGENTS.md:----------", "big:", "climb:L---------", "dir:d---------", "in:L---------", "out:L---------"} {
					if !strings.Contains(got, w) {
						t.Errorf("listing %q lacks %q", got, w)
					}
				}
				if !strings.HasPrefix(got, "AGENTS.md:") {
					t.Errorf("listing %q is not sorted", got)
				}
			}
		})
	}
	if _, err := os.Lstat(filepath.Join(s.dir, "fifo")); err == nil {
		start := time.Now()
		if _, err := fs.ReadFile(fsys, "fifo"); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("a FIFO: %v", err)
		}
		if time.Since(start) > 10*time.Second {
			t.Error("the read of a FIFO waited on it")
		}
	}
	// A directory's file system over the wire behaves as io/fs says.
	sub, err := fs.Sub(fsys, "dir")
	if err != nil {
		t.Fatal(err)
	}
	if err := fstest.TestFS(sub, "a.txt", "sub/b.txt"); err != nil {
		t.Error(err)
	}
}

// readRaw sends a resources/read for uri, as a client that skips the
// client's checks would, and returns the reply.
func readRaw(t *testing.T, cs *sdk.ClientSession, uri string) (fileReply, error) {
	t.Helper()
	res, err := cs.ReadResource(context.Background(), &sdk.ReadResourceParams{URI: uri})
	if err != nil {
		return fileReply{}, err
	}
	if len(res.Contents) != 1 || res.Contents[0].MIMEType != "application/json" {
		t.Fatalf("contents %+v", res.Contents)
	}
	var r fileReply
	if err := json.Unmarshal([]byte(res.Contents[0].Text), &r); err != nil {
		t.Fatal(err)
	}
	return r, nil
}

// The server checks every request itself, whatever the client did: it
// refuses an invalid name, "..", an absolute name and a link out, an op
// it does not have (a write among them) and a URI with anything more
// (which the template does not match, or the server refuses), and
// writes nothing.
func TestTheServerRefusesWhatLeavesOrWrites(t *testing.T) {
	s, _ := filesBox(t)
	cs := s.connect(t).Session()
	before := tree(t, s.dir)
	for _, tc := range []struct {
		uri  string
		kind string
	}{
		{"dax-workspace:///read?path=..%2Fsecret", kindOutside},
		{"dax-workspace:///stat?path=..", kindOutside},
		{"dax-workspace:///read?path=%2Fetc%2Fpasswd", kindOutside},
		{"dax-workspace:///read?path=out", kindOutside},
		{"dax-workspace:///stat?path=climb", kindOutside},
		{"dax-workspace:///readdir?path=dir%2F..%2F..", kindOutside},
		{"dax-workspace:///read?path=dir%2F..%2FAGENTS.md", kindInvalid},
		{"dax-workspace:///read?path=dir%2F", kindInvalid},
		{"dax-workspace:///read?path=", kindInvalid},
		{"dax-workspace:///read", kindInvalid},
		{"dax-workspace:///write?path=new.txt", kindInvalid},
		{"dax-workspace:///remove?path=AGENTS.md", kindInvalid},
		{"dax-workspace:///read?path=AGENTS.md&data=x", ""},
		{"dax-workspace:///read?path=AGENTS.md&path=big", ""},
		{"dax-workspace://host/read?path=AGENTS.md", ""},
		{"dax-workspace:///read?path=missing", kindNotExist},
		{"dax-workspace:///read?path=big", kindOther},
	} {
		t.Run(tc.uri, func(t *testing.T) {
			r, err := readRaw(t, cs, tc.uri)
			if tc.kind == "" {
				// Not matched by the template (not found), or invalid.
				if err == nil && (r.Error == nil || r.Error.Kind != kindInvalid) {
					t.Errorf("reply %+v, want a refusal", r)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if r.Error == nil || r.Error.Kind != tc.kind || r.Data != nil || r.Info != nil {
				t.Errorf("reply %+v, want a %s error", r, tc.kind)
			}
		})
	}
	// A URI the template does not match is not found.
	if _, err := cs.ReadResource(context.Background(), &sdk.ReadResourceParams{URI: "file:///etc/passwd"}); err == nil {
		t.Error("file:///etc/passwd was read")
	}
	if r, err := readRaw(t, cs, fileURI(opRead, "AGENTS.md")); err != nil || string(r.Data) != "Be brief.\n" {
		t.Errorf("a good read: %+v %v", r, err)
	}
	if after := tree(t, s.dir); !slices.Equal(before, after) {
		t.Errorf("the workspace changed:\n%v\n%v", before, after)
	}
}

// tree is every name under dir with its size and mode.
func tree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, p+" "+fi.Mode().String()+" "+fi.ModTime().String())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A name is percent-encoded byte by byte outside the unreserved set,
// which the template's {?path} matches, and the server decodes it back.
func TestFileURIsRoundTrip(t *testing.T) {
	s := newServer(t)
	cs := s.connect(t).Session()
	for _, name := range []string{"a b+c&d=e?f#g%h", "ünï/cødé.md", "dir/sub/x~y_z-1.2"} {
		writeFile(t, filepath.Join(s.dir, filepath.FromSlash(name)), name)
		uri := fileURI(opRead, name)
		if op, got, err := parseFileURI(uri); err != nil || op != opRead || got != name {
			t.Errorf("parse %s = %s %q %v", uri, op, got, err)
		}
		if r, err := readRaw(t, cs, uri); err != nil || string(r.Data) != name {
			t.Errorf("read %s: %+v %v", uri, r, err)
		}
	}
}

// A listing whose names would walk somewhere else is refused by the
// client, whatever the executor sends.
func TestTheClientRefusesABadListing(t *testing.T) {
	for _, name := range []string{"..", ".", "", "a/b", "dup"} {
		t.Run(name, func(t *testing.T) {
			entries := []fileInfo{{Name: name}}
			if name == "dup" {
				entries = append(entries, fileInfo{Name: name})
			}
			srv := sdk.NewServer(&sdk.Implementation{Name: "liar", Version: "1"}, nil)
			srv.AddResourceTemplate(&sdk.ResourceTemplate{Name: "f", URITemplate: FilesURITemplate}, func(context.Context, *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
				data, _ := json.Marshal(fileReply{Entries: entries})
				return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{Text: string(data)}}}, nil
			})
			ctx := context.Background()
			st, ct := sdk.NewInMemoryTransports()
			ss, err := srv.Connect(ctx, st, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer ss.Close()
			cs, err := sdk.NewClient(&sdk.Implementation{Name: "c", Version: "1"}, nil).Connect(ctx, ct, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer cs.Close()
			if es, err := fs.ReadDir(remoteFS{cs}, "."); err == nil {
				t.Errorf("listing %v accepted", es)
			}
		})
	}
}

// A request the executor does not answer in time fails, as does one to
// an executor that is gone; neither waits on.
func TestAFilesRequestThatCannotBeAnsweredFails(t *testing.T) {
	old := filesTimeout
	filesTimeout = 100 * time.Millisecond
	t.Cleanup(func() { filesTimeout = old })
	s, _ := filesBox(t)
	stall := make(chan struct{})
	t.Cleanup(func() { close(stall) })
	var stalled atomic.Bool
	s.srv.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			if method == "resources/read" && stalled.Load() {
				select {
				case <-stall:
				case <-ctx.Done():
				}
			}
			return next(ctx, method, req)
		}
	})
	r, ss := s.remote(t)
	if _, err := fs.ReadFile(r.FS(), "AGENTS.md"); err != nil {
		t.Fatal(err)
	}
	stalled.Store(true)
	start := time.Now()
	if _, err := fs.Stat(r.FS(), "AGENTS.md"); err == nil || !strings.Contains(err.Error(), "executor") {
		t.Errorf("a stalled executor: %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("waited %v", d)
	}
	stalled.Store(false)
	ss.Close()
	if _, err := fs.ReadFile(r.FS(), "AGENTS.md"); err == nil || !strings.Contains(err.Error(), "executor") {
		t.Errorf("an executor that is gone: %v", err)
	}
}
