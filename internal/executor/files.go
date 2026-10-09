package executor

// The executor's workspace files, read-only, over MCP resources: what a
// session reads of the project where the project is (AGENTS.md,
// .dax/skills, .dax/config.json), not on its own machine.
//
// One resource template serves them:
//
//	dax-workspace:///{op}{?path}
//
// op is stat, lstat, readdir, readlink or read, and path a name in the
// workspace's file system (fs.ValidPath: relative to its root, slash
// separated, "." for the root), percent-encoded. Every name goes
// through the workspace's FS, which is confined to its root (os.Root
// for dax execute's workspace.Local): a name that leaves it, by ".."
// or a link, is refused, and nothing is ever written.
//
// A read gives at most MaxFileBytes of a regular file and refuses a
// larger one, a directory, a FIFO or a device; a listing gives at most
// MaxDirEntries entries and refuses a longer one. The reply is one
// application/json text content, a fileReply; an error is in it, with a
// kind the client maps back to fs.ErrNotExist, agentworkspace's
// ErrOutside, fs.ErrInvalid or fs.ErrPermission, or a message alone.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	workspace "github.com/ChristopherDavenport/agentworkspace"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// FilesScheme is the URI scheme of the workspace's files.
const FilesScheme = "dax-workspace"

// FilesURITemplate is the resource template the executor serves its
// workspace's files under, and names in its capability.
const FilesURITemplate = FilesScheme + ":///{op}{?path}"

// MaxFileBytes bounds one read of a file: the bound dax already puts on
// a project's config (config.MaxProjectBytes) and the skill tool on a
// skill's file (agentskill.DefaultMaxBytes), the largest project file a
// session reads. AGENTS.md is held to a far smaller budget, checked by
// its size before it is read.
const MaxFileBytes = 1 << 20

// MaxDirEntries bounds one listing, so a reply stays a few megabytes.
const MaxDirEntries = 10000

// The operations of FilesURITemplate.
const (
	opStat     = "stat"
	opLstat    = "lstat"
	opReadDir  = "readdir"
	opReadLink = "readlink"
	opRead     = "read"
)

var fileOps = []string{opStat, opLstat, opReadDir, opReadLink, opRead}

// The kinds of a fileError.
const (
	kindNotExist   = "notexist"
	kindOutside    = "outside"
	kindInvalid    = "invalid"
	kindPermission = "permission"
	kindOther      = "other"
)

// fileReply is the JSON of one reply: the field of its op, or Error.
type fileReply struct {
	Info    *fileInfo  `json:"info,omitempty"`    // stat, lstat
	Entries []fileInfo `json:"entries,omitempty"` // readdir, as lstat describes each
	Target  string     `json:"target,omitempty"`  // readlink
	Data    []byte     `json:"data,omitempty"`    // read, base64
	Error   *fileError `json:"error,omitempty"`
}

// fileInfo is an fs.FileInfo on the wire; Mode is Go's fs.FileMode.
type fileInfo struct {
	Name    string      `json:"name"`
	Size    int64       `json:"size"`
	Mode    fs.FileMode `json:"mode"`
	ModTime time.Time   `json:"modTime"`
}

// fileError is a refusal: Kind is notexist, outside, invalid,
// permission or other, Message the executor's words.
type fileError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// maxErrorBytes bounds an error's message, which is the sandbox's text.
const maxErrorBytes = 512

func infoOf(fi fs.FileInfo) fileInfo {
	return fileInfo{Name: fi.Name(), Size: fi.Size(), Mode: fi.Mode(), ModTime: fi.ModTime()}
}

// fileErrorOf is err as a reply's error.
func fileErrorOf(err error) *fileError {
	kind := kindOther
	switch {
	case errors.Is(err, workspace.ErrOutside):
		kind = kindOutside
	case errors.Is(err, fs.ErrNotExist):
		kind = kindNotExist
	case errors.Is(err, fs.ErrInvalid):
		kind = kindInvalid
	case errors.Is(err, fs.ErrPermission):
		kind = kindPermission
	}
	msg := err.Error()
	if len(msg) > maxErrorBytes {
		msg = msg[:maxErrorBytes]
	}
	return &fileError{Kind: kind, Message: strings.ToValidUTF8(msg, "?")}
}

// checkName refuses a name that is not fs.ValidPath: one that is
// absolute or climbs out with ".." leaves the workspace and is
// ErrOutside; any other is fs.ErrInvalid. The workspace's FS checks
// again; this keeps the refusal the server's whatever FS it serves.
func checkName(op, name string) error {
	if fs.ValidPath(name) {
		return nil
	}
	if c := path.Clean(name); path.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../") {
		return &fs.PathError{Op: op, Path: name, Err: workspace.ErrOutside}
	}
	return &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
}

// fileURI is the URI of op on name, every byte outside RFC 3986's
// unreserved set percent-encoded, as the template's {?path} matches.
func fileURI(op, name string) string {
	var b strings.Builder
	b.WriteString(FilesScheme + ":///" + op + "?path=")
	for i := 0; i < len(name); i++ {
		c := name[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// parseFileURI is a request's op and name. A URI with anything else in
// it is fs.ErrInvalid.
func parseFileURI(uri string) (op, name string, err error) {
	bad := func(why string) (string, string, error) {
		return "", "", &fs.PathError{Op: "resource", Path: uri, Err: fmt.Errorf("%w: %s", fs.ErrInvalid, why)}
	}
	u, err := url.Parse(uri)
	if err != nil {
		return bad(err.Error())
	}
	if u.Scheme != FilesScheme || u.Host != "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return bad("not a " + FilesURITemplate + " URI")
	}
	op = strings.TrimPrefix(u.Path, "/")
	if !slices.Contains(fileOps, op) {
		return bad(fmt.Sprintf("op %q: want one of %s", op, strings.Join(fileOps, ", ")))
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return bad(err.Error())
	}
	if len(q) != 1 || len(q["path"]) != 1 {
		return bad("want one path and nothing else")
	}
	name = q["path"][0]
	if !utf8.ValidString(name) {
		return bad("the path is not UTF-8")
	}
	return op, name, nil
}

// serveFiles registers the workspace's files on srv, read through fsys.
func serveFiles(srv *sdk.Server, fsys fs.FS) {
	srv.AddResourceTemplate(&sdk.ResourceTemplate{
		Name:        "workspace-files",
		Title:       "The workspace's files, read-only",
		Description: "op is stat, lstat, readdir, readlink or read; path a name relative to the workspace's root. Reads are bounded; nothing is written.",
		MIMEType:    "application/json",
		URITemplate: FilesURITemplate,
	}, func(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
		reply := fileOp(fsys, req.Params.URI)
		data, err := json.Marshal(reply)
		if err != nil {
			return nil, err
		}
		res := &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: req.Params.URI, MIMEType: "application/json", Text: string(data)}}}
		// The files change under the session; a client keeps no copy.
		res.CacheScope = "private"
		return res, nil
	})
}

// fileOp answers one request.
func fileOp(fsys fs.FS, uri string) fileReply {
	fail := func(err error) fileReply { return fileReply{Error: fileErrorOf(err)} }
	op, name, err := parseFileURI(uri)
	if err != nil {
		return fail(err)
	}
	if err := checkName(op, name); err != nil {
		return fail(err)
	}
	switch op {
	case opStat, opLstat:
		stat := fs.Stat
		if op == opLstat {
			stat = fs.Lstat
		}
		fi, err := stat(fsys, name)
		if err != nil {
			return fail(err)
		}
		info := infoOf(fi)
		return fileReply{Info: &info}
	case opReadDir:
		es, err := fs.ReadDir(fsys, name)
		if err != nil {
			return fail(err)
		}
		if len(es) > MaxDirEntries {
			return fail(fmt.Errorf("%s holds %d entries, over the %d a listing through the executor gives", name, len(es), MaxDirEntries))
		}
		out := make([]fileInfo, 0, len(es))
		for _, e := range es {
			fi, err := e.Info()
			if errors.Is(err, fs.ErrNotExist) {
				continue // removed since the listing
			}
			if err != nil {
				return fail(err)
			}
			out = append(out, infoOf(fi))
		}
		return fileReply{Entries: out}
	case opReadLink:
		t, err := fs.ReadLink(fsys, name)
		if err != nil {
			return fail(err)
		}
		return fileReply{Target: t}
	default: // opRead
		data, err := readBounded(fsys, name)
		if err != nil {
			return fail(err)
		}
		return fileReply{Data: data}
	}
}

// readBounded reads a regular file of at most MaxFileBytes, opened
// through fsys, which does not block on a FIFO.
func readBounded(fsys fs.FS, name string) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, &fs.PathError{Op: "read", Path: name, Err: errors.New("not a regular file")}
	}
	tooBig := func(n int64) error {
		return &fs.PathError{Op: "read", Path: name, Err: fmt.Errorf("%d bytes, over the %d-byte limit of a read through the executor", n, MaxFileBytes)}
	}
	if fi.Size() > MaxFileBytes {
		return nil, tooBig(fi.Size())
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFileBytes {
		return nil, tooBig(int64(len(data)))
	}
	return data, nil
}

// filesTimeout bounds one request for the workspace's files. A request
// that takes longer fails as a read error does. A var for the tests.
var filesTimeout = 30 * time.Second

// remoteFS is the executor's workspace files as an fs.FS, read through
// FilesURITemplate. It answers Stat, Lstat, ReadDir, ReadLink and
// ReadFile with one request each; Open stats the name, and a file's
// contents or a directory's entries are read on the first Read or
// ReadDir. It refuses an invalid name before asking, and checks what
// the executor sends back: a read over MaxFileBytes and a listing with a
// name that is not one element are errors.
type remoteFS struct{ s *sdk.ClientSession }

var (
	_ fs.StatFS     = remoteFS{}
	_ fs.ReadDirFS  = remoteFS{}
	_ fs.ReadFileFS = remoteFS{}
	_ fs.ReadLinkFS = remoteFS{}
)

// errFiles wraps a failure to reach the executor's files.
var errFiles = errors.New("executor")

// do sends op on name and returns the reply, its error mapped back.
func (f remoteFS) do(op, name string) (fileReply, error) {
	if err := checkName(op, name); err != nil {
		return fileReply{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), filesTimeout)
	defer cancel()
	res, err := f.s.ReadResource(ctx, &sdk.ReadResourceParams{URI: fileURI(op, name)})
	if err != nil {
		return fileReply{}, &fs.PathError{Op: op, Path: name, Err: fmt.Errorf("%w: %w", errFiles, err)}
	}
	if len(res.Contents) != 1 || res.Contents[0] == nil {
		return fileReply{}, &fs.PathError{Op: op, Path: name, Err: fmt.Errorf("%w: %d contents in the reply, want 1", errFiles, len(res.Contents))}
	}
	var r fileReply
	if err := json.Unmarshal([]byte(res.Contents[0].Text), &r); err != nil {
		return fileReply{}, &fs.PathError{Op: op, Path: name, Err: fmt.Errorf("%w: the reply: %w", errFiles, err)}
	}
	if e := r.Error; e != nil {
		return fileReply{}, &fs.PathError{Op: op, Path: name, Err: kindErr(e)}
	}
	return r, nil
}

// kindErr is a reply's error as this side's: its kind's sentinel,
// wrapped with the executor's message, which is shown as its words.
func kindErr(e *fileError) error {
	msg := e.Message
	if len(msg) > maxErrorBytes {
		msg = msg[:maxErrorBytes]
	}
	msg = strings.ToValidUTF8(msg, "?")
	var sentinel error
	switch e.Kind {
	case kindNotExist:
		sentinel = fs.ErrNotExist
	case kindOutside:
		sentinel = workspace.ErrOutside
	case kindInvalid:
		sentinel = fs.ErrInvalid
	case kindPermission:
		sentinel = fs.ErrPermission
	default:
		return fmt.Errorf("%w: %s", errFiles, msg)
	}
	return &remoteErr{sentinel: sentinel, msg: msg}
}

// remoteErr is an error the executor gave: its words, and the sentinel
// its kind maps to.
type remoteErr struct {
	sentinel error
	msg      string
}

func (e *remoteErr) Error() string { return "executor: " + e.msg }
func (e *remoteErr) Unwrap() error { return e.sentinel }

// info is a reply's fileInfo as an fs.FileInfo named name.
func (r fileReply) info(op, name string) (fs.FileInfo, error) {
	if r.Info == nil {
		return nil, &fs.PathError{Op: op, Path: name, Err: fmt.Errorf("%w: no info in the reply", errFiles)}
	}
	i := *r.Info
	i.Name = path.Base(name)
	return remoteInfo{i}, nil
}

func (f remoteFS) Stat(name string) (fs.FileInfo, error) {
	r, err := f.do(opStat, name)
	if err != nil {
		return nil, err
	}
	return r.info(opStat, name)
}

func (f remoteFS) Lstat(name string) (fs.FileInfo, error) {
	r, err := f.do(opLstat, name)
	if err != nil {
		return nil, err
	}
	return r.info(opLstat, name)
}

func (f remoteFS) ReadLink(name string) (string, error) {
	r, err := f.do(opReadLink, name)
	if err != nil {
		return "", err
	}
	if r.Target == "" {
		return "", &fs.PathError{Op: opReadLink, Path: name, Err: fmt.Errorf("%w: no target in the reply", errFiles)}
	}
	return r.Target, nil
}

// ReadDir lists name, sorted by name. An entry whose name is not one
// element of a path ("", ".", "..", or one with a slash) fails it,
// since a walk would join it into a name somewhere else.
func (f remoteFS) ReadDir(name string) ([]fs.DirEntry, error) {
	r, err := f.do(opReadDir, name)
	if err != nil {
		return nil, err
	}
	if len(r.Entries) > MaxDirEntries {
		return nil, &fs.PathError{Op: opReadDir, Path: name, Err: fmt.Errorf("%w: %d entries, over %d", errFiles, len(r.Entries), MaxDirEntries)}
	}
	out := make([]fs.DirEntry, 0, len(r.Entries))
	seen := map[string]bool{}
	for _, e := range r.Entries {
		if e.Name == "." || !fs.ValidPath(e.Name) || strings.Contains(e.Name, "/") || seen[e.Name] {
			return nil, &fs.PathError{Op: opReadDir, Path: name, Err: fmt.Errorf("%w: the listing names %q", errFiles, e.Name)}
		}
		seen[e.Name] = true
		out = append(out, fs.FileInfoToDirEntry(remoteInfo{e}))
	}
	slices.SortFunc(out, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return out, nil
}

func (f remoteFS) ReadFile(name string) ([]byte, error) {
	r, err := f.do(opRead, name)
	if err != nil {
		return nil, err
	}
	if len(r.Data) > MaxFileBytes {
		return nil, &fs.PathError{Op: opRead, Path: name, Err: fmt.Errorf("%w: %d bytes, over the %d-byte limit", errFiles, len(r.Data), MaxFileBytes)}
	}
	return r.Data, nil
}

// Open stats name, following links inside the workspace; the file
// reads its contents or entries when first asked.
func (f remoteFS) Open(name string) (fs.File, error) {
	fi, err := f.Stat(name)
	if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			pe.Op = "open"
		}
		return nil, err
	}
	return &remoteFile{fs: f, name: name, info: fi}, nil
}

// remoteInfo is a fileInfo as an fs.FileInfo.
type remoteInfo struct{ i fileInfo }

func (r remoteInfo) Name() string       { return r.i.Name }
func (r remoteInfo) Size() int64        { return r.i.Size }
func (r remoteInfo) Mode() fs.FileMode  { return r.i.Mode }
func (r remoteInfo) ModTime() time.Time { return r.i.ModTime }
func (r remoteInfo) IsDir() bool        { return r.i.Mode.IsDir() }
func (r remoteInfo) Sys() any           { return nil }

// remoteFile is an opened name: its Stat, and its contents or entries
// read on first use.
type remoteFile struct {
	fs   remoteFS
	name string
	info fs.FileInfo

	data    *strings.Reader
	entries []fs.DirEntry
	listed  bool
	closed  bool
}

func (f *remoteFile) Stat() (fs.FileInfo, error) {
	if f.closed {
		return nil, &fs.PathError{Op: "stat", Path: f.name, Err: fs.ErrClosed}
	}
	return f.info, nil
}

func (f *remoteFile) Read(p []byte) (int, error) {
	switch {
	case f.closed:
		return 0, &fs.PathError{Op: "read", Path: f.name, Err: fs.ErrClosed}
	case f.info.IsDir():
		return 0, &fs.PathError{Op: "read", Path: f.name, Err: errors.New("is a directory")}
	}
	if f.data == nil {
		data, err := f.fs.ReadFile(f.name)
		if err != nil {
			return 0, err
		}
		f.data = strings.NewReader(string(data))
	}
	return f.data.Read(p)
}

func (f *remoteFile) ReadDir(n int) ([]fs.DirEntry, error) {
	switch {
	case f.closed:
		return nil, &fs.PathError{Op: "readdir", Path: f.name, Err: fs.ErrClosed}
	case !f.info.IsDir():
		return nil, &fs.PathError{Op: "readdir", Path: f.name, Err: errors.New("not a directory")}
	}
	if !f.listed {
		es, err := f.fs.ReadDir(f.name)
		if err != nil {
			return nil, err
		}
		f.entries, f.listed = es, true
	}
	if n <= 0 {
		out := f.entries
		f.entries = nil
		return out, nil
	}
	if len(f.entries) == 0 {
		return nil, io.EOF
	}
	n = min(n, len(f.entries))
	out := f.entries[:n:n]
	f.entries = f.entries[n:]
	return out, nil
}

func (f *remoteFile) Close() error {
	if f.closed {
		return &fs.PathError{Op: "close", Path: f.name, Err: fs.ErrClosed}
	}
	f.closed = true
	return nil
}
