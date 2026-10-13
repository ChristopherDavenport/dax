package dax

// This file is dax serve -a2a: agentturn's front/a2a mounted beside the
// served session's control, so another agent (a dax with the dax-a2a
// extension, or any A2A client) can call this one. Each A2A context is
// a conversation of its own, run on an agent of its own under the
// served session's configuration (agent.Session.PeerConfig), recorded
// in a dax session of its own (agent.Session.OpenPeer), and never
// written into the served one: one session, one conversation.
//
// The JSON-RPC endpoint is behind the token the control is, under the
// same principal, and the agent card is served unauthenticated at the
// well-known path so a caller can learn the scheme. The conversations,
// who owns each, and the calls a task put to its caller are kept in a
// private directory, so a restart keeps them.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agentturn"
	fronta2a "github.com/ChristopherDavenport/agentturn/front/a2a"
	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/internal/private"
)

// a2aPath is where front/a2a's JSON-RPC is served; the card is at
// a2asrv.WellKnownAgentCardPath.
const a2aPath = "/a2a"

// servePrincipal is who holds serve's token, on the control and on A2A
// alike, so the record names one principal for both.
const servePrincipal = "attach"

// a2aPeers is front/a2a over a served session: the executor, the card,
// and the conversations' runs, which end when serving does.
type a2aPeers struct {
	sess  *agent.Session
	store *a2aStore
	exec  *fronta2a.Executor
	card  *a2a.AgentCard

	// stop ends every conversation's run; serve cancels it when it
	// stops.
	stop context.Context

	mu      sync.Mutex
	records map[string]*agent.PeerRecord // by context ID
	closing bool
	runs    sync.WaitGroup
}

// a2aOptions are what serve mounts front/a2a with.
type a2aOptions struct {
	// url is the served base URL, which the card names when a request
	// does not say where it reached serve.
	url, version string
	// dir is where the conversations are kept.
	dir string
	// tokens map each bearer token to its principal: serve's one token
	// to servePrincipal.
	tokens map[string]string
	// escalate puts a call the policy asks about to the caller
	// (front/a2a's WithEscalation), in place of the served session's
	// fronts.
	escalate bool
	// stop ends the conversations' runs.
	stop context.Context
}

// newA2APeers builds front/a2a over sess.
func newA2APeers(ctx context.Context, sess *agent.Session, o a2aOptions) (*a2aPeers, error) {
	store, err := openA2AStore(o.dir)
	if err != nil {
		return nil, err
	}
	p := &a2aPeers{sess: sess, store: store, stop: o.stop, records: map[string]*agent.PeerRecord{}}
	cfg := sess.PeerConfig()
	opts := []fronta2a.Option{
		fronta2a.WithConversationStore(store),
		fronta2a.WithAuthenticator(fronta2a.BearerTokens(o.tokens)),
		fronta2a.WithRecorderFor(p.recorderFor),
	}
	if o.escalate {
		opts = append(opts, fronta2a.WithEscalation())
	}
	p.exec = fronta2a.New(cfg, opts...)
	p.card = p.exec.Secure(fronta2a.AgentCard(ctx, cfg, o.url+a2aPath, o.version))
	return p, nil
}

// mount serves the JSON-RPC endpoint and the card on mux.
func (p *a2aPeers) mount(mux *http.ServeMux) {
	mux.Handle(a2aPath, p.exec.Handler(a2asrv.NewJSONRPCHandler(p.exec.RequestHandler())))
	mux.HandleFunc(a2asrv.WellKnownAgentCardPath, p.serveCard)
}

// serveCard serves the card naming the endpoint at the origin the
// request reached, a tunnel's end or the name a certificate is for,
// which a caller holds the endpoint to (dax-a2a does), where the
// address serve listens on may be neither. The card carries no secret,
// so the request's Host names only what the caller already used.
func (p *a2aPeers) serveCard(w http.ResponseWriter, r *http.Request) {
	card := *p.card
	if r.Host != "" {
		u := url.URL{Scheme: "http", Host: r.Host, Path: a2aPath}
		if r.TLS != nil {
			u.Scheme = "https"
		}
		card.URL = u.String()
	}
	w.Header().Set("Cache-Control", "no-store")
	a2asrv.NewStaticAgentCardHandler(&card).ServeHTTP(w, r)
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

// record is the conversation's session: the one this process opened,
// the one the store says records it, or a new one, which the store
// then names.
func (p *a2aPeers) record(ctx context.Context, contextID string) (*agent.PeerRecord, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if rec, ok := p.records[contextID]; ok {
		return rec, nil
	}
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
	p.records[contextID] = rec
	return rec, nil
}

// close refuses new runs and waits, for at most drain, for those
// going, which stop has ended, so their ends are on the record before
// the session closes.
func (p *a2aPeers) close(drain time.Duration) {
	p.mu.Lock()
	p.closing = true
	p.mu.Unlock()
	done := make(chan struct{})
	go func() {
		p.runs.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(drain):
	}
}

// a2aDir is where the conversations of the session served in dir, and
// recorded in the store at root, are kept: under the user's dax
// directory, one directory per workspace and store, so a restart in the
// same place continues them and one elsewhere, or recording elsewhere,
// does not find sessions its store lacks.
func a2aDir(root, dir string) string {
	sum := sha256.Sum256([]byte(root + "\x00" + dir))
	return filepath.Join(agent.DefaultUserDir(), "a2a", hex.EncodeToString(sum[:8]))
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
	mu  sync.Mutex
}

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

func openA2AStore(dir string) (*a2aStore, error) {
	for _, d := range []string{dir, filepath.Join(dir, "conversations"), filepath.Join(dir, "owners")} {
		if err := private.SecureDir(d); err != nil {
			return nil, fmt.Errorf("a2a: %w", err)
		}
	}
	return &a2aStore{dir: dir}, nil
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
// read, renamed into place.
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
	return os.Rename(tmp, path)
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
	if err := os.Link(tmp, path); err != nil && !errors.Is(err, os.ErrExist) {
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
