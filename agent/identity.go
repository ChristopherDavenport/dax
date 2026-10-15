package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	workspace "github.com/ChristopherDavenport/agentworkspace"
)

// A session's project is its workspace: where its tools act, as the
// workspace's descriptor names it (kind, ref, root). The header's cwd
// is the root alone, which says nothing of the machine: /work in one
// container and /work in another are different projects, and a
// checkout here is not the same path in the sandbox its tools ran in.
// So every session dax starts records its workspace in its header, and
// with an executor, which address reached it, so that -list can find
// the project an executor serves without connecting to it.
const (
	// headerWorkspace is the header member holding the session's
	// workspace: {"kind", "ref", "root"}, ref left out when empty.
	headerWorkspace = "dax.workspace"
	// headerExecutor is the header member holding, for a session whose
	// tools ran through an executor, {"sha256": hex}: the hash of the
	// address it was reached by (executorCommandID, executorURLID).
	// The address itself is never written: a session file is exported
	// and shared, and a command line can carry what its user would not
	// want in one.
	headerExecutor = "dax.executor"
)

// recordedWorkspace is a workspace as the header holds it.
type recordedWorkspace struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref,omitempty"`
	Root string `json:"root"`
}

// executorMark is an executor's address as the header holds it.
type executorMark struct {
	SHA256 string `json:"sha256"`
}

// executorCommandID is the identity of an executor started by the
// program argv[0] with the arguments argv[1:]: the hash of the argv,
// so the command line a config file gives as a string and the one it
// gives as an array are one executor when they split alike.
func executorCommandID(argv []string) string {
	return addressID(struct {
		Command []string `json:"command"`
	}{argv})
}

// executorURLID is the identity of an executor serving at endpoint:
// the hash of the URL with its scheme and host in lower case and a
// path of "/" dropped, which name the same server.
func executorURLID(endpoint string) string {
	return addressID(struct {
		URL string `json:"url"`
	}{normalURL(endpoint)})
}

func addressID(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// normalURL is endpoint as it identifies a server: trimmed, its scheme
// and host in lower case, and a path that is only "/" dropped.
func normalURL(endpoint string) string {
	s := strings.TrimSpace(endpoint)
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return s
	}
	u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	if u.Path == "/" {
		u.Path, u.RawPath = "", ""
	}
	return u.String()
}

// extras are the header members dax records of a new session: its
// workspace, and the address of the executor its tools run through.
func (s *Session) extras() map[string]json.RawMessage {
	d := s.x.Descriptor()
	out := map[string]json.RawMessage{}
	if ws, err := json.Marshal(recordedWorkspace{Kind: d.Kind, Ref: d.Ref, Root: d.Root}); err == nil {
		out[headerWorkspace] = ws
	}
	if ex := s.opts.Executor; ex != nil && ex.addr != "" {
		if mark, err := json.Marshal(executorMark{SHA256: ex.addr}); err == nil {
			out[headerExecutor] = mark
		}
	}
	return out
}

// recordedIn is the workspace a header records, and whether it records
// one.
func recordedIn(h agentsession.Header) (workspace.Descriptor, bool) {
	raw, ok := h.Extra[headerWorkspace]
	if !ok {
		return workspace.Descriptor{}, false
	}
	var w recordedWorkspace
	if err := json.Unmarshal(raw, &w); err != nil || w.Kind == "" {
		return workspace.Descriptor{}, false
	}
	return workspace.Descriptor{Kind: w.Kind, Ref: w.Ref, Root: w.Root}, true
}

// workspaceOf is the workspace a header records, or, for a session
// recorded before dax wrote one, this machine at its cwd.
func workspaceOf(h agentsession.Header) workspace.Descriptor {
	if d, ok := recordedIn(h); ok {
		return d
	}
	return workspace.Descriptor{Kind: workspace.KindLocal, Root: h.CWD}
}

// where names a workspace for a line: this machine's as its root
// alone, any other as its kind, its ref and its root ("container
// box:/work").
func where(d workspace.Descriptor) string {
	if d.Kind == workspace.KindLocal && d.Ref == "" {
		return d.Root
	}
	w := d.Kind
	if d.Ref != "" {
		w += " " + d.Ref
	}
	return w + ":" + d.Root
}

// List returns the sessions recorded under root whose tools acted in
// dir on this machine, newest first: those whose header records that
// workspace, and those recorded before dax wrote one whose cwd is dir.
// A session whose tools ran in an executor is not one of them, even
// when the executor's root is the same path; ListExecutor finds those.
// An empty dir lists every session.
func List(ctx context.Context, root, dir string) ([]agentsession.Summary, error) {
	if dir == "" {
		return list(ctx, root, agentsession.ListFilter{}, nil)
	}
	here := workspace.Descriptor{Kind: workspace.KindLocal, Root: dir}
	return list(ctx, root, agentsession.ListFilter{CWD: dir}, &here)
}

// ListExecutor returns the sessions of the project an executor started
// by argv serves, as DialExecutor would start it, without starting it:
// the workspace the latest session reached by that command recorded,
// and every session recorded in that workspace, newest first, whichever
// address reached it. A command no recorded session used lists
// nothing, and the descriptor is zero. Sessions recorded before dax
// wrote the address are not found.
//
// The address stands for the machine it reached last: one that has
// since led to another (a tunnel pointed at another box) lists the
// latest one's project, and the others are still opened by ID.
func ListExecutor(ctx context.Context, root string, argv []string) (workspace.Descriptor, []agentsession.Summary, error) {
	return listExecutor(ctx, root, executorCommandID(argv))
}

// ListExecutorURL is ListExecutor for an executor serving at endpoint,
// as DialExecutorURL reaches it.
func ListExecutorURL(ctx context.Context, root, endpoint string) (workspace.Descriptor, []agentsession.Summary, error) {
	return listExecutor(ctx, root, executorURLID(endpoint))
}

func listExecutor(ctx context.Context, root, id string) (workspace.Descriptor, []agentsession.Summary, error) {
	var none workspace.Descriptor
	if !cas.IsStore(root) {
		return none, nil, nil
	}
	store, err := readOnly(root)
	if err != nil {
		return none, nil, err
	}
	defer store.Close()
	mark, err := json.Marshal(executorMark{SHA256: id})
	if err != nil {
		return none, nil, err
	}
	// The store lists newest first, so the first is the latest.
	var (
		ws      workspace.Descriptor
		found   bool
		scanErr error
	)
	for sum, err := range store.List(ctx, agentsession.ListFilter{Extra: map[string]json.RawMessage{headerExecutor: mark}}) {
		if err != nil {
			scanErr = err
			break
		}
		if d, ok := recordedIn(sum.Header); ok {
			ws, found = d, true
			break
		}
	}
	if !found {
		return none, nil, scanErr
	}
	sums, err := listIn(ctx, store, agentsession.ListFilter{CWD: ws.Root}, &ws)
	return ws, sums, errors.Join(scanErr, err)
}

// list opens the store under root for reading and lists it.
func list(ctx context.Context, root string, f agentsession.ListFilter, in *workspace.Descriptor) ([]agentsession.Summary, error) {
	if !cas.IsStore(root) {
		return nil, nil
	}
	store, err := readOnly(root)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	return listIn(ctx, store, f, in)
}

// listIn is the sessions f keeps, and of those, when in is not nil,
// the ones whose workspace is in. A sub-agent's session records only
// its parent's cwd, so it is in its parent's workspace when its parent
// is listed beside it, as it always is, since the two share the cwd.
// It stops at the first error, with what it listed.
func listIn(ctx context.Context, store agentsession.Store, f agentsession.ListFilter, in *workspace.Descriptor) ([]agentsession.Summary, error) {
	var all []agentsession.Summary
	var err error
	for sum, e := range store.List(ctx, f) {
		if e != nil {
			err = e
			break
		}
		all = append(all, sum)
	}
	if in == nil {
		return all, err
	}
	byID := make(map[string]agentsession.Header, len(all))
	for _, s := range all {
		byID[s.Header.ID] = s.Header
	}
	of := func(h agentsession.Header) workspace.Descriptor {
		// The depth bounds a chain a damaged store made into a loop.
		for range 64 {
			if d, ok := recordedIn(h); ok {
				return d
			}
			p, ok := byID[h.ParentSession]
			if h.SpawnedBy == "" || !ok {
				break
			}
			h = p
		}
		return workspaceOf(h)
	}
	var out []agentsession.Summary
	for _, s := range all {
		if of(s.Header) == *in {
			out = append(out, s)
		}
	}
	return out, err
}
