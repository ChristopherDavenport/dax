package authn

// This file is the tunnel's proof, both ways: the workspace end
// (`dax workspace -connect`) and the session's hub share a token, and
// neither sends it. Before each upgrade the workspace end asks the hub
// for a nonce (a plain GET of the hub's URL), which the hub issues
// without keeping anything: the nonce carries when it was issued and
// the hub's MAC of that under a secret of its own, made at its start
// and never the token. The upgrade carries the workspace end's HMAC,
// under the token, of the hub's name, that nonce and the run's instance
// id; the hub takes it only for a nonce it issued, younger than
// NonceLifetime, and once. Its 101 carries its own HMAC of the same,
// under another label, which the workspace end checks before it serves
// anything.
//
// What this proves, over ws:// to loopback behind an ssh -R forward
// where a local user may take the port first: the impostor learns no
// token, and the upgrade it captured is made over a nonce the impostor
// handed out, which no hub issued, so it is good for nothing later,
// through the forward or at another hub (the name, and the hub's own
// secret, bind it to one hub). What it does not prove: it does not
// bind the connection. An impostor that can reach the hub while it
// holds the port (another forward, the hub's host) relays the GET and
// the upgrade at once and sits between the two on a plain connection;
// and one that fetched a nonce from the hub while the forward was up,
// then takes the port after a drop, can hand that nonce out and replay
// the upgrade it gets within the nonce's lifetime. Only wss:// binds the
// connection; where the hub is reachable otherwise, use it.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// ProofScheme is the upgrade's Authorization scheme:
	//	Authorization: Dax-Tunnel nonce="<nonce>", proof="<mac>"
	ProofScheme = "Dax-Tunnel"
	// NonceHeader carries the nonce the hub issues, on its answer to a
	// GET of its URL.
	NonceHeader = "Dax-Tunnel-Nonce"
	// HubProofHeader carries the hub's MAC on its 101.
	HubProofHeader = "Dax-Hub-Proof"
	// ProofChallenge is the hub's WWW-Authenticate on a 401.
	ProofChallenge = ProofScheme + ` realm="dax"`
	// NonceLifetime is how long a nonce the hub issued may be used,
	// by the hub's own monotonic clock: the other host's clock does
	// not matter.
	NonceLifetime = 30 * time.Second
	// MaxLiveNonces bounds the nonces a hub holds as taken; past it an
	// upgrade is ErrBusy, until the oldest expire. Only an upgrade
	// proved with the token adds one.
	MaxLiveNonces = 1024

	workspaceLabel = "dax-tunnel workspace v2"
	hubLabel       = "dax-tunnel hub v2"
	nonceLabel     = "dax-tunnel nonce v2"
	maxNonceBytes  = 128
)

// Errors of the hub's check; each is a refusal.
var (
	ErrNoProof       = errors.New("authn: the upgrade carries no tunnel proof")
	ErrBadProof      = errors.New("authn: the upgrade's proof is not made with the hub's token and name")
	ErrUnknownNonce  = errors.New("authn: the upgrade's proof is over a nonce this hub did not issue (a replay of an upgrade made for another hub, or for whatever held the port)")
	ErrStaleNonce    = fmt.Errorf("authn: the upgrade's nonce is more than %s old", NonceLifetime)
	ErrReplayedNonce = errors.New("authn: the upgrade's nonce was used before")
	ErrBusy          = fmt.Errorf("authn: %d nonces are taken and live, the most there may be", MaxLiveNonces)
	// ErrHubProof is a hub whose answer does not prove it holds the
	// token.
	ErrHubProof = errors.New("authn: the hub's answer does not prove it holds the token")
	// ErrNoNonce is a hub that answered the nonce's GET without one: not
	// a dax hub, or one older than this proof.
	ErrNoNonce = errors.New("authn: the hub issued no nonce")
)

// mac is the hex HMAC-SHA256 under key of label and parts, each ended
// by a zero byte, which none of them holds.
func mac(key []byte, label string, parts ...string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(label + "\x00"))
	for _, p := range parts {
		m.Write([]byte(p + "\x00"))
	}
	return hex.EncodeToString(m.Sum(nil))
}

// validNonce reports whether s may be a nonce at all: short, of the
// characters a hub's nonce has, so it cannot make a MAC's input
// ambiguous or a header long.
func validNonce(s string) bool {
	if s == "" || len(s) > maxNonceBytes {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'A' || c > 'Z') && (c < 'a' || c > 'f') && c != '.' {
			return false
		}
	}
	return true
}

// proofOf reads the upgrade's Authorization: exactly
// `Dax-Tunnel nonce="<nonce>", proof="<mac>"`, as Prover writes it.
func proofOf(r *http.Request) (nonce, proof string, ok bool) {
	rest, ok := strings.CutPrefix(r.Header.Get("Authorization"), ProofScheme+` nonce="`)
	if !ok {
		return "", "", false
	}
	nonce, rest, ok = strings.Cut(rest, `", proof="`)
	if !ok {
		return "", "", false
	}
	proof, ok = strings.CutSuffix(rest, `"`)
	if !ok || len(proof) != 2*sha256.Size || !validNonce(nonce) {
		return "", "", false
	}
	return nonce, proof, true
}

// FetchNonce asks the hub at hubURL (its ws:// or wss:// URL) for a
// nonce, through hc: a GET of the same URL over http or https. A
// failure to reach it is an error to try again on; an answer without a
// nonce is ErrNoNonce.
func FetchNonce(ctx context.Context, hc *http.Client, hubURL string) (string, error) {
	u := hubURL
	switch {
	case strings.HasPrefix(u, "wss://"):
		u = "https://" + strings.TrimPrefix(u, "wss://")
	case strings.HasPrefix(u, "ws://"):
		u = "http://" + strings.TrimPrefix(u, "ws://")
	default:
		return "", errors.New("authn: the hub's URL is not ws:// or wss://")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", errors.New("authn: the hub's URL is not one a request can be made to")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("authn: asking the hub for a nonce: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	nonce := resp.Header.Get(NonceHeader)
	if resp.StatusCode != http.StatusOK || !validNonce(nonce) {
		return "", fmt.Errorf("%w (it answered %s)", ErrNoNonce, resp.Status)
	}
	return nonce, nil
}

// Prover is the workspace end's half: it proves each upgrade over the
// nonce the hub issued and checks the hub's answer. The token is the
// key, never sent; name is the workspace's name the hub enrolls, which
// both MACs cover, so a proof for one hub is no proof at another that
// holds the same token.
type Prover struct {
	key  []byte
	name string
}

// NewProver is the proof under token, for the hub that enrolls name.
func NewProver(token, name string) *Prover {
	return &Prover{key: []byte(token), name: name}
}

// Header is the header of one upgrade: nonce, as the hub issued it, and
// the workspace end's MAC of the name, it and bound (the run's instance
// id), in Authorization.
func (p *Prover) Header(nonce, bound string) http.Header {
	return http.Header{"Authorization": {fmt.Sprintf(`%s nonce="%s", proof="%s"`, ProofScheme, nonce, mac(p.key, workspaceLabel, p.name, nonce, bound))}}
}

// CheckHub reports whether resp, the answer to req (as sent, with
// Header's Authorization), carries the hub's MAC of the name, req's
// nonce and bound under the token; ErrHubProof otherwise.
func (p *Prover) CheckHub(req *http.Request, resp *http.Response, bound string) error {
	nonce, _, ok := proofOf(req)
	if !ok {
		return fmt.Errorf("%w: the upgrade carried no nonce", ErrHubProof)
	}
	got := resp.Header.Get(HubProofHeader)
	if got == "" {
		return fmt.Errorf("%w: it sent none", ErrHubProof)
	}
	if !hmac.Equal([]byte(got), []byte(mac(p.key, hubLabel, p.name, nonce, bound))) {
		return ErrHubProof
	}
	return nil
}

// Hub is the hub's half: its token and name, the secret its nonces are
// made under, and the nonces it has taken that are still live.
type Hub struct {
	key    []byte
	name   string
	secret []byte
	since  func() time.Duration // the hub's age, by the monotonic clock

	mu    sync.Mutex
	taken map[string]time.Duration // a nonce taken, and the age it expires at
	order []string                 // taken, oldest first
}

// NewHub is the hub's proof for the one token, enrolling name; its
// nonces' secret is random, for this hub's life.
func NewHub(token, name string) *Hub {
	secret := make([]byte, 32)
	rand.Read(secret)
	start := time.Now()
	return &Hub{key: []byte(token), name: name, secret: secret, since: func() time.Duration { return time.Since(start) }, taken: map[string]time.Duration{}}
}

// Nonce issues a nonce: the hub's age in nanoseconds, 26 random base32
// characters, and the hub's MAC of both under its secret, dot-separated.
// It keeps nothing.
func (h *Hub) Nonce() string {
	body := strconv.FormatInt(int64(h.since()), 10) + "." + rand.Text()
	return body + "." + mac(h.secret, nonceLabel, body)[:32]
}

// issued is the age at which the hub issued nonce, when it did.
func (h *Hub) issued(nonce string) (time.Duration, bool) {
	i := strings.LastIndexByte(nonce, '.')
	if i < 0 {
		return 0, false
	}
	body, sum := nonce[:i], nonce[i+1:]
	if !hmac.Equal([]byte(sum), []byte(mac(h.secret, nonceLabel, body)[:32])) {
		return 0, false
	}
	ns, _, _ := strings.Cut(body, ".")
	n, err := strconv.ParseInt(ns, 10, 64)
	if err != nil {
		return 0, false
	}
	return time.Duration(n), true
}

// verify checks r's proof of bound: made with the token and name, over
// a nonce this hub issued, within its lifetime. The MAC is checked
// first, so every later refusal is of an upgrade made with the token.
func (h *Hub) verify(r *http.Request, bound string) (nonce string, expires time.Duration, err error) {
	nonce, proof, ok := proofOf(r)
	if !ok {
		return "", 0, ErrNoProof
	}
	if !hmac.Equal([]byte(proof), []byte(mac(h.key, workspaceLabel, h.name, nonce, bound))) {
		return "", 0, ErrBadProof
	}
	at, ok := h.issued(nonce)
	if !ok {
		return "", 0, ErrUnknownNonce
	}
	if age := h.since() - at; age < 0 || age > NonceLifetime {
		return "", 0, ErrStaleNonce
	}
	return nonce, at + NonceLifetime, nil
}

// Check reports whether r's proof of bound would be taken, without
// taking its nonce: a look before the enrollment (Take) decides.
func (h *Hub) Check(r *http.Request, bound string) error {
	_, _, err := h.verify(r, bound)
	return err
}

// Take is Check, and takes the nonce, so a proof is good for one
// upgrade: one taken before is ErrReplayedNonce. A taken nonce is kept
// until it expires, the expired ones dropped oldest first; with
// MaxLiveNonces live, Take is ErrBusy.
func (h *Hub) Take(r *http.Request, bound string) error {
	nonce, expires, err := h.verify(r, bound)
	if err != nil {
		return err
	}
	now := h.since()
	h.mu.Lock()
	defer h.mu.Unlock()
	// Taken in about the order issued, so the expired are at the front;
	// one out of order waits behind a live one at most a lifetime.
	for len(h.order) > 0 && h.taken[h.order[0]] < now {
		delete(h.taken, h.order[0])
		h.order = h.order[1:]
	}
	if _, used := h.taken[nonce]; used {
		return ErrReplayedNonce
	}
	if len(h.taken) >= MaxLiveNonces {
		return ErrBusy
	}
	h.taken[nonce] = expires
	h.order = append(h.order, nonce)
	return nil
}

// Header is the hub's answer to r, a proof Take accepted: its MAC of the
// name, r's nonce and bound under the token, in HubProofHeader. It
// checks r's proof again, so it answers nothing it did not take.
func (h *Hub) Header(r *http.Request, bound string) (http.Header, error) {
	nonce, _, err := h.verify(r, bound)
	if err != nil {
		return nil, err
	}
	return http.Header{HubProofHeader: {mac(h.key, hubLabel, h.name, nonce, bound)}}, nil
}
