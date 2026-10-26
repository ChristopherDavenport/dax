package dax

// This file is dax serve -a2a: agentturn's front/a2a mounted beside the
// served session's control, so another agent (a dax with the dax-a2a
// extension, or any A2A client) can call this one. Each A2A context is
// a conversation of its own, run on an agent of its own under the
// served session's configuration (agent.Session.PeerConfig), recorded
// in a dax session of its own (agent.Session.OpenPeer), and never
// written into the served one: one session, one conversation.
//
// The JSON-RPC endpoint and the agent card are behind a token of their
// own, under the principal a2aPrincipal, which the control and the
// store do not take: an A2A caller reaches the agent and nothing else of
// the session, and serve's token does not reach A2A. The
// conversations, who owns each, the calls a task put to its caller and
// the task each waits on are kept in a private directory, so a restart
// keeps them, and one dax serve at a time keeps them there.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agentturn"
	fronta2a "github.com/ChristopherDavenport/agentturn/front/a2a"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/internal/authn"
	"github.com/ChristopherDavenport/dax/internal/private"
)

// a2aPath is where front/a2a's JSON-RPC is served; the card is at
// a2asrv.WellKnownAgentCardPath.
const a2aPath = "/a2a"

// servePrincipal is who holds serve's token, on the control and the
// store; a2aPrincipal is who holds the A2A token, on A2A alone, which
// the record names as the caller of each conversation's runs.
const (
	servePrincipal = "attach"
	a2aPrincipal   = "a2a"
)

// a2aTokenEnv names the variable that holds the A2A token. Without it,
// dax serve -a2a makes one and writes it beside serve's.
const a2aTokenEnv = "DAX_SERVE_A2A_TOKEN"

// a2aPeers is front/a2a over a served session: the executor, the card,
// and the conversations' runs, which end when serving does.
type a2aPeers struct {
	sess    *agent.Session
	store   *a2aStore
	exec    *fronta2a.Executor
	cfg     agentturn.Config
	version string
	// base is the URL the card names the endpoint under (-a2a-url); nil
	// is the origin each request reached.
	base *url.URL

	// stop ends every conversation's run: serve cancels it when it
	// stops, and so does close (endRuns).
	stop    context.Context
	endRuns context.CancelFunc

	mu      sync.Mutex
	closing bool
	runs    sync.WaitGroup
}

// a2aOptions are what serve mounts front/a2a with.
type a2aOptions struct {
	// base is the public base URL the card names (-a2a-url), checked by
	// checkA2AURL; nil names the origin each request reached.
	base    *url.URL
	version string
	// dir is where the conversations are kept.
	dir string
	// tokens map each bearer token to its principal: the A2A token to
	// a2aPrincipal.
	tokens map[string]string
	// escalate puts a call the policy asks about to the caller
	// (front/a2a's WithEscalation), in place of the served session's
	// views.
	escalate bool
	// stop ends the conversations' runs.
	stop context.Context
}

// newA2APeers builds front/a2a over sess. It holds o.dir until close.
func newA2APeers(sess *agent.Session, o a2aOptions) (*a2aPeers, error) {
	store, err := openA2AStore(o.dir)
	if err != nil {
		return nil, err
	}
	p := &a2aPeers{sess: sess, store: store, cfg: sess.PeerConfig(), version: o.version, base: o.base}
	p.stop, p.endRuns = context.WithCancel(o.stop)
	opts := []fronta2a.Option{
		fronta2a.WithConversationStore(store),
		// The scheme is named on the card explicitly, whatever front/a2a's
		// default name.
		fronta2a.WithAuthenticator(fronta2a.Authenticator{
			Name:      "bearer",
			Scheme:    a2a.HTTPAuthSecurityScheme{Scheme: "Bearer"},
			Principal: authn.New(o.tokens).Principal,
		}),
		fronta2a.WithRecorderFor(p.recorderFor),
	}
	if o.escalate {
		opts = append(opts, fronta2a.WithEscalation())
	}
	p.exec = fronta2a.New(p.cfg, opts...)
	return p, nil
}

// mount serves the JSON-RPC endpoint and the card on mux, both behind
// the A2A token, while serve takes A2A work (accepting).
func (p *a2aPeers) mount(mux *http.ServeMux) {
	mux.Handle(a2aPath, p.accepting(p.exec.Handler(a2asrv.NewJSONRPCHandler(p.exec.RequestHandler()))))
	mux.Handle("GET "+a2asrv.WellKnownAgentCardPath, p.accepting(p.exec.Handler(http.HandlerFunc(p.serveCard))))
}

// accepting serves a request with next until close, and counts it among
// the work close waits for; from close on, it is refused with 503.
func (p *a2aPeers) accepting(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		if p.closing {
			p.mu.Unlock()
			http.Error(w, "dax serve is stopping", http.StatusServiceUnavailable)
			return
		}
		p.runs.Add(1)
		p.mu.Unlock()
		defer p.runs.Done()
		next.ServeHTTP(w, r)
	})
}

// card is the agent card a request is answered with: the agent's tools
// as they are now, an MCP server added since serve started included,
// and the endpoint at -a2a-url, else at the origin the request reached,
// which a caller holds the endpoint to (dax-a2a does).
func (p *a2aPeers) card(r *http.Request) *a2a.AgentCard {
	u := url.URL{Scheme: "http", Host: r.Host, Path: a2aPath}
	if r.TLS != nil {
		u.Scheme = "https"
	}
	if p.base != nil {
		u = *p.base
		u.Path = strings.TrimSuffix(u.Path, "/") + a2aPath
	}
	return p.exec.Secure(fronta2a.AgentCard(r.Context(), p.cfg, u.String(), p.version))
}

// serveCard writes the card as JSON. It is served to the token's holder
// only, with none of the cross-origin headers a2a-go's card handler
// sends: no page in a browser has a use for it.
func (p *a2aPeers) serveCard(w http.ResponseWriter, r *http.Request) {
	data, err := json.Marshal(p.card(r))
	if err != nil {
		http.Error(w, "the agent card", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}

// checkA2AURL checks -a2a-url, the base URL a caller reaches serve at,
// which the card names: https, or http to loopback, with a host and no
// credentials, query or fragment. A path is a prefix a proxy strips.
func checkA2AURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("-a2a-url: %w", err)
	}
	switch {
	case u.Host == "" || u.Opaque != "":
		return nil, fmt.Errorf("-a2a-url %q: want an absolute URL with a host", raw)
	case u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return nil, fmt.Errorf("-a2a-url %q: want no credentials, query or fragment", raw)
	case u.Scheme == "https":
	case u.Scheme == "http" && loopbackHost(u.Hostname()):
	default:
		return nil, fmt.Errorf("-a2a-url %q: want https, or http to loopback; a token sent in the clear to another host is anyone's", raw)
	}
	return u, nil
}

// loopbackHost reports whether host names this machine's loopback.
func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// recorderFor is front/a2a's RecorderFor: the conversation's run is
// recorded in its session, its questions name the caller and the
// conversation, and it ends when serving does.
func (p *a2aPeers) recorderFor(ctx context.Context, contextID string, a *agentturn.Agent) (context.Context, func(), error) {
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return nil, nil, errors.New("dax serve is stopping")
	}
	p.runs.Add(1)
	p.mu.Unlock()
	rec, err := p.record(ctx, contextID)
	if err != nil {
		p.runs.Done()
		return nil, nil, err
	}
	who := "A2A caller"
	if principal, ok := fronta2a.PrincipalFrom(ctx); ok {
		who += " " + principal
	}
	// The context ID is the caller's to choose: quoted and cut short, it
	// cannot pass for the rest of the question a view is shown.
	conv := contextID
	if len(conv) > 64 {
		conv = conv[:64] + "..."
	}
	rctx, detach, err := rec.Attach(ctx, a, who+", conversation "+strconv.Quote(conv))
	if err != nil {
		p.runs.Done()
		return nil, nil, err
	}
	rctx, cancel := context.WithCancel(rctx)
	unhook := context.AfterFunc(p.stop, cancel)
	return rctx, func() {
		unhook()
		cancel()
		detach()
		p.runs.Done()
	}, nil
}

// record opens the conversation's session for one task: the one the
// store says records it, or a new one, which the store then names.
// Nothing is kept of it after the task, so a conversation costs nothing
// between its messages. front/a2a runs one task of a conversation at a
// time, so no two open one conversation's session at once, and no lock
// of this process is held across the store's reads and writes.
func (p *a2aPeers) record(ctx context.Context, contextID string) (*agent.PeerRecord, error) {
	id, err := p.store.sessionOf(contextID)
	if err != nil {
		return nil, err
	}
	rec, err := p.sess.OpenPeer(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("the conversation's session: %w", err)
	}
	if id == "" {
		if err := p.store.setSession(contextID, rec.ID()); err != nil {
			return nil, err
		}
	}
	return rec, nil
}

// close stops taking A2A work first: a new request is refused, and so
// is a new run. It then ends the runs going and waits, for at most
// drain, for them and the requests in flight, so their ends are on the
// record before the session closes. Last, the store refuses every write
// and lets go of the directory, so nothing this process does writes
// there after another serve may take it.
func (p *a2aPeers) close(drain time.Duration) {
	p.mu.Lock()
	p.closing = true
	p.mu.Unlock()
	p.endRuns()
	done := make(chan struct{})
	go func() {
		p.runs.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(drain):
	}
	p.store.close()
}

// a2aDir is where the conversations of a session are kept: under the
// user's dax directory, one directory per session store (root, as the
// store resolves it), working directory on this machine (host, where
// dax serve was started, so two projects served on one executor image
// are two places) and workspace (its kind, ref and root, so two
// containers with one root are two places), so a restart in the same
// place continues them and one elsewhere does not find sessions its
// store lacks.
func a2aDir(root, host string, ws workspace.Descriptor) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{root, host, ws.Kind, ws.Ref, ws.Root}, "\x00")))
	return filepath.Join(agent.DefaultUserDir(), "a2a", hex.EncodeToString(sum[:8]))
}

// storeRoot is the root of the session's store, resolved, so two
// spellings of one directory are one key: the store's own when it says,
// else flag, the -sessions it was opened from.
func storeRoot(sess *agent.Session, flag string) string {
	root := flag
	if r, ok := sess.Recorder().Store().(interface{ Root() string }); ok {
		root = r.Root()
	}
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	return root
}

// a2aStore keeps what front/a2a must not lose to a restart, in files
// under a directory only the user can read: each conversation's
// transcript, the calls its last task put to its caller, the task it
// waits on and the session that records it, in one file replaced whole
// on each change, and who owns each task and conversation. It is
// front/a2a's ConversationStore, EscalationStore, WaitingStore and
// Owners.
type a2aStore struct {
	dir string
	// mu is held by every read-change-write and every claim; closed,
	// under it, refuses writes once close has begun to let go.
	mu     sync.Mutex
	closed bool
	// lock holds the directory's lock file, locked for the store's life.
	lock *os.File
}

// errStoreClosed is a write after close.
var errStoreClosed = errors.New("a2a: dax serve is stopping; the conversations are no longer written")

var (
	_ fronta2a.ConversationStore = (*a2aStore)(nil)
	_ fronta2a.EscalationStore   = (*a2aStore)(nil)
	_ fronta2a.WaitingStore      = (*a2aStore)(nil)
	_ fronta2a.Owners            = (*a2aStore)(nil)
)

// a2aConversation is one conversation's file.
type a2aConversation struct {
	ContextID  string               `json:"context_id"`
	Session    string               `json:"session,omitempty"`
	Transcript agentturn.Transcript `json:"transcript,omitempty"`
	Escalated  []fronta2a.Escalated `json:"escalated,omitempty"`
	Waiting    string               `json:"waiting,omitempty"`
}

// openA2AStore opens the store in dir and locks it: a second store
// over dir, in this process or another dax serve, is refused until
// close, so two never write one conversation. What a write cut short
// left (.tmp-* files) is swept, which the lock makes safe.
func openA2AStore(dir string) (*a2aStore, error) {
	subs := []string{filepath.Join(dir, "conversations"), filepath.Join(dir, "owners")}
	for _, d := range append([]string{dir}, subs...) {
		if err := private.SecureDir(d); err != nil {
			return nil, fmt.Errorf("a2a: %w", err)
		}
	}
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("a2a: %w", err)
	}
	if err := lockFile(lock); err != nil {
		lock.Close()
		return nil, lockError(dir, err)
	}
	for _, d := range subs {
		left, _ := filepath.Glob(filepath.Join(d, ".tmp-*"))
		for _, f := range left {
			os.Remove(f)
		}
	}
	return &a2aStore{dir: dir, lock: lock}, nil
}

// errLockHeld is a lock another open file holds.
var errLockHeld = errors.New("the lock is held")

// lockError says why the directory could not be locked: another dax
// serve holds it, or the system's own error, which a file system
// without locks gives.
func lockError(dir string, err error) error {
	if errors.Is(err, errLockHeld) {
		return fmt.Errorf("a2a: another dax serve -a2a keeps its conversations in %s; stop it, or serve elsewhere", dir)
	}
	return fmt.Errorf("a2a: locking %s: %w", dir, err)
}

// close refuses every later write, then lets go of the directory.
func (s *a2aStore) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.lock != nil {
		s.lock.Close()
		s.lock = nil
	}
}

// hashedName is a file's name for a key a caller chose: a hash, so no ID
// names a path.
func hashedName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func (s *a2aStore) conversationFile(contextID string) string {
	return filepath.Join(s.dir, "conversations", hashedName(contextID)+".json")
}

// load reads the conversation's file; a missing one is an empty
// conversation. s.mu is held.
func (s *a2aStore) load(contextID string) (a2aConversation, error) {
	c := a2aConversation{ContextID: contextID}
	data, err := os.ReadFile(s.conversationFile(contextID))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("a2a conversation %q: %w", contextID, err)
	}
	if c.ContextID != contextID {
		return a2aConversation{ContextID: contextID}, fmt.Errorf("a2a conversation %q: the file holds %q", contextID, c.ContextID)
	}
	for _, e := range c.Escalated {
		// A decision with no rewrite is written with "Args": null,
		// which decodes as the four bytes null; read back so, the
		// approved call would run with null for its arguments.
		if d := e.Decision; d != nil && string(d.Args) == "null" {
			d.Args = nil
		}
	}
	return c, nil
}

// update reads, changes and writes the conversation's file whole, a
// rename over the old one.
func (s *a2aStore) update(contextID string, change func(*a2aConversation)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errStoreClosed
	}
	c, err := s.load(contextID)
	if err != nil {
		return err
	}
	change(&c)
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return writePrivate(s.conversationFile(contextID), data)
}

// writePrivate writes data to path through a file only the user can
// read, renamed into place, and the rename made durable.
func writePrivate(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// syncDir makes the names in dir durable, a rename or a link into it.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Load implements fronta2a.ConversationStore.
func (s *a2aStore) Load(_ context.Context, contextID string) (agentturn.Transcript, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.load(contextID)
	return c.Transcript, err
}

// Save implements fronta2a.ConversationStore.
func (s *a2aStore) Save(_ context.Context, contextID string, t agentturn.Transcript) error {
	return s.update(contextID, func(c *a2aConversation) { c.Transcript = t })
}

// LoadEscalated implements fronta2a.EscalationStore.
func (s *a2aStore) LoadEscalated(_ context.Context, contextID string) ([]fronta2a.Escalated, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.load(contextID)
	return c.Escalated, err
}

// SaveEscalated implements fronta2a.EscalationStore.
func (s *a2aStore) SaveEscalated(_ context.Context, contextID string, calls []fronta2a.Escalated) error {
	return s.update(contextID, func(c *a2aConversation) { c.Escalated = calls })
}

// LoadWaiting implements fronta2a.WaitingStore.
func (s *a2aStore) LoadWaiting(_ context.Context, contextID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.load(contextID)
	return c.Waiting, err
}

// SaveWaiting implements fronta2a.WaitingStore.
func (s *a2aStore) SaveWaiting(_ context.Context, contextID, task string) error {
	return s.update(contextID, func(c *a2aConversation) { c.Waiting = task })
}

// sessionOf is the session that records the conversation; empty for
// none yet.
func (s *a2aStore) sessionOf(contextID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.load(contextID)
	return c.Session, err
}

func (s *a2aStore) setSession(contextID, id string) error {
	return s.update(contextID, func(c *a2aConversation) { c.Session = id })
}

func (s *a2aStore) ownerFile(key string) string {
	return filepath.Join(s.dir, "owners", hashedName(key))
}

// Owner implements fronta2a.Owners.
func (s *a2aStore) Owner(_ context.Context, key string) (string, bool, error) {
	data, err := os.ReadFile(s.ownerFile(key))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(data), true, nil
}

// Claim implements fronta2a.Owners: the owner's file is linked into
// place whole, which fails when the key has one, so two claims, in this
// process or another, agree on the first.
func (s *a2aStore) Claim(ctx context.Context, key, principal string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", errStoreClosed
	}
	path := s.ownerFile(key)
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, err = f.WriteString(principal)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if err := os.Link(tmp, path); err == nil {
		if err := syncDir(filepath.Dir(path)); err != nil {
			return "", err
		}
	} else if !errors.Is(err, os.ErrExist) {
		return "", err
	}
	owner, ok, err := s.Owner(ctx, key)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("a2a: the owner of %q was not kept", key)
	}
	return owner, nil
}
