// Package memory is dax-memory, the extension that gives the model a
// memory between sessions: a store with the user's scope and one for
// each project, the workspace the tools act in, the memory tools, and
// the memory block in the system prompt. It is built only from what package extension offers
// any extension, and dax's private-directory check.
package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"path"
	"path/filepath"
	"strings"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentmemory/filestore"
	"github.com/ChristopherDavenport/agenttool"
	workspace "github.com/ChristopherDavenport/agentworkspace"

	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/internal/private"
	"github.com/ChristopherDavenport/dax/policy"
)

// Name is the extension's name, and so its rules' source:
// extension:dax-memory.
const Name = "dax-memory"

// New is dax-memory over the store in dir, which is made private (0700)
// when the session starts, with a warning if it was not. The model
// reads and writes a user scope and the scope of the project the tools
// act in (WorkspaceScope), and an archive beside each (ArchiveScope),
// which it adds passages to with memory_insert and finds with
// memory_search, and which the block never shows. It owns the memory
// tools and conversation_search, which finds what an earlier session
// of this project said (Env.SearchScope: none for an A2A caller), and
// ships one rule: the searches run unasked, while a save, a patch, a
// forget and an insert ask. The sub-agents take memory_search
// over the same scopes (SubagentTools), and see the block the kit
// renders as its parts; they are offered no writer. WithBackground
// adds the background pass after a run (#160).
func New(dir string, opts ...Option) extension.Extension {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	var after func(extension.Env) (func(context.Context, extension.Run), error)
	if m := o.background.Mode; m == BackgroundAuto || m == BackgroundPropose {
		after = func(env extension.Env) (func(context.Context, extension.Run), error) {
			p := &passer{dir: dir, env: env, b: o.background}
			return p.after, nil
		}
	}
	return extension.Extension{
		AfterRun: after,
		Name:     Name,
		Owns:     []string{agentmemory.SaveTool, agentmemory.PatchTool, agentmemory.ForgetTool, agentmemory.SearchTool, agentmemory.InsertTool, agentkit.ConversationSearchTool},
		Policy:   policy.Rules{Allow: []string{agentmemory.SearchTool, agentkit.ConversationSearchTool}},
		// The memory is the person's: an A2A caller is refused every
		// memory tool outright (conversation_search refuses it through
		// Env.SearchScope), and the store reads empty on its context.
		PersonOnly: []string{agentmemory.SaveTool, agentmemory.PatchTool, agentmemory.ForgetTool, agentmemory.SearchTool, agentmemory.InsertTool},
		SubagentTools: func(env extension.Env) ([]agenttool.Tool, error) {
			mem, scopes, err := open(dir, env)
			if err != nil {
				return nil, err
			}
			for _, t := range agentmemory.Tools(mem, append(scopes, archives(scopes)...)) {
				if t.Name() == agentmemory.SearchTool {
					return []agenttool.Tool{t}, nil
				}
			}
			return nil, nil
		},
		Kit: func(env extension.Env) ([]agentkit.Option, error) {
			mem, scopes, err := open(dir, env)
			if err != nil {
				return nil, err
			}
			arch := archives(scopes)
			return []agentkit.Option{
				// conversation_search, over this project's earlier
				// sessions, scoped by the caller: never an A2A
				// caller's.
				agentkit.WithSessionSearch(agentkit.SearchScope(env.SearchScope)),
				agentkit.WithMemory(mem, scopes...),
				agentkit.WithMemoryArchive(arch...),
				agentkit.WithMemoryTools(agentmemory.WithInsert(arch...)),
			}, nil
		},
	}
}

// Option is a choice New makes beyond the store's directory.
type Option func(*options)

type options struct{ background Background }

// WithBackground runs the background pass b after the person's runs:
// the person's own setting (memory.background), which a project cannot
// make.
func WithBackground(b Background) Option {
	return func(o *options) { o.background = b }
}

// archives are the archive scopes beside scopes, in order.
func archives(scopes []agentmemory.Scope) []agentmemory.Scope {
	out := make([]agentmemory.Scope, len(scopes))
	for i, sc := range scopes {
		out[i] = ArchiveScope(sc)
	}
	return out
}

// open is the store in dir, made private, and the session's scopes:
// the user's and the project's the tools act in.
func open(dir string, env extension.Env) (agentmemory.Store, []agentmemory.Scope, error) {
	if err := private.SecureDir(dir); err != nil {
		return nil, nil, fmt.Errorf("memory: %w", err)
	}
	fs, err := filestore.Open(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("memory: %w", err)
	}
	mem := personal{Store: fs, caller: env.Caller}
	project := ProjectScope(env.Dir())
	if ws := env.ToolEnv().Workspace; ws != nil {
		project = WorkspaceScope(ws.Descriptor(), env.Dir())
	}
	return mem, []agentmemory.Scope{"user", project}, nil
}

// ProjectScope is the memory scope of a working directory. One store
// holds every project's memory beside the user's, so each project gets
// a scope of its own, named for its directory and a hash of its path.
func ProjectScope(dir string) agentmemory.Scope {
	sum := sha256.Sum256([]byte(dir))
	return scope(filepath.Base(dir), sum[:])
}

// WorkspaceScope is the memory scope of the project a session's tools
// act in, d, for a session started in dir on this machine. It is
// exported so that a program built on dax that reads the store finds
// a session's project where dax-memory keeps it.
//
// This machine's workspace (kind local and no ref, which is what
// workspace.Local says) is ProjectScope(dir), the scope a project had
// before the workspace decided, so the memory kept then is still the
// project's. Any other, a container or a host an executor runs on, is
// named for the base of its root with a hash of its kind, ref and root:
// two sandboxes started from one directory here are two projects, and
// one sandbox is one project from wherever dax was started.
func WorkspaceScope(d workspace.Descriptor, dir string) agentmemory.Scope {
	if d.Kind == workspace.KindLocal && d.Ref == "" {
		return ProjectScope(dir)
	}
	// A JSON array, so no kind, ref or root can be read as another's.
	key, _ := json.Marshal([]string{d.Kind, d.Ref, d.Root})
	sum := sha256.Sum256(key)
	return scope(path.Base(filepath.ToSlash(d.Root)), sum[:])
}

// scope is a project's scope: "project-", base in lower-case kebab
// form, and the first four bytes of sum in hex.
func scope(base string, sum []byte) agentmemory.Scope {
	var b strings.Builder
	b.WriteString("project-")
	for _, r := range strings.ToLower(base) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
		if b.Len() >= 40 {
			break
		}
	}
	if !strings.HasSuffix(b.String(), "-") {
		b.WriteByte('-')
	}
	b.WriteString(hex.EncodeToString(sum[:4]))
	return agentmemory.Scope(b.String())
}

// ArchiveScope is the archive beside scope: user-archive beside the
// user's, project-…-archive beside a project's. An archive is written
// with memory_insert and found with memory_search, and the block never
// shows it (agentkit.WithMemoryArchive), so it holds what is worth
// finding again and not worth reading every turn.
func ArchiveScope(scope agentmemory.Scope) agentmemory.Scope {
	return scope + "-archive"
}

// errCaller refuses a write to the person's memory on an A2A caller's
// context.
var errCaller = errors.New("the memory is the person's: an A2A caller cannot change it")

// personal is the store as the session reads it: the person's own, so
// on an A2A caller's context, or its sub-agent's (Env.Caller), it
// reads empty and refuses every write. The kit renders the memory
// block through it for each request, so a caller's requests carry no
// block, and its conversation records no manifest of the person's
// entries; the memory tools are refused to a caller before they run
// (PersonOnly), and this holds behind them.
//
// It is a filestore's, and keeps the store's own search options
// (agentmemory.Searcher) and its last sequence number
// (agentmemory.LastSeqer), so the person's memory_search takes the
// same options through it.
type personal struct {
	*filestore.Store
	caller func(context.Context) (string, bool)
}

var (
	_ agentmemory.Searcher  = personal{}
	_ agentmemory.LastSeqer = personal{}
)

func (p personal) isCaller(ctx context.Context) bool {
	_, ok := p.caller(ctx)
	return ok
}

func (p personal) Get(ctx context.Context, scope agentmemory.Scope, name string) (*agentmemory.Entry, error) {
	if p.isCaller(ctx) {
		return nil, agentmemory.ErrNotFound
	}
	return p.Store.Get(ctx, scope, name)
}

func (p personal) List(ctx context.Context, scope agentmemory.Scope) ([]agentmemory.Entry, error) {
	if p.isCaller(ctx) {
		return nil, nil
	}
	return p.Store.List(ctx, scope)
}

func (p personal) Search(ctx context.Context, scopes []agentmemory.Scope, query string, limit int) ([]agentmemory.Entry, error) {
	if p.isCaller(ctx) {
		return nil, nil
	}
	return p.Store.Search(ctx, scopes, query, limit)
}

func (p personal) Put(ctx context.Context, e agentmemory.Entry, opts ...agentmemory.PutOption) (*agentmemory.Change, error) {
	if p.isCaller(ctx) {
		return nil, errCaller
	}
	return p.Store.Put(ctx, e, opts...)
}

func (p personal) Forget(ctx context.Context, scope agentmemory.Scope, name string) (*agentmemory.Change, error) {
	if p.isCaller(ctx) {
		return nil, errCaller
	}
	return p.Store.Forget(ctx, scope, name)
}

func (p personal) Journal(ctx context.Context, after uint64) iter.Seq2[agentmemory.Change, error] {
	if p.isCaller(ctx) {
		return func(func(agentmemory.Change, error) bool) {}
	}
	return p.Store.Journal(ctx, after)
}

func (p personal) SearchWith(ctx context.Context, scopes []agentmemory.Scope, query string, limit int, opts agentmemory.SearchOptions) ([]agentmemory.Entry, error) {
	if p.isCaller(ctx) {
		return nil, nil
	}
	return p.Store.SearchWith(ctx, scopes, query, limit, opts)
}

func (p personal) LastSeq(ctx context.Context) (uint64, error) {
	if p.isCaller(ctx) {
		return 0, nil
	}
	return p.Store.LastSeq(ctx)
}
