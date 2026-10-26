package authn

// This file is the tunnel's proof, both ways, as agentworkspace's
// tunnel package doc lays it out ("Proving the hub"): the workspace end
// and the hub share a token, and neither sends it. Each upgrade carries
// a fresh nonce and the workspace end's HMAC of it under the token,
// which the hub's enrollment checks, refusing a nonce too old or seen
// before; the hub answers with its own HMAC of the same nonce, under
// another label, which the workspace end checks before it serves
// anything. What crosses the wire is good for one dial alone, so over
// ws:// an impostor holding the port learns nothing it can use again.
//
// The proof authenticates the hub at the handshake and does not bind
// the connection: an impostor that can reach the real hub relays the
// upgrade at once and passes. It holds where the impostor cannot reach
// the hub (ws:// to loopback behind an ssh -R forward); where it can,
// only wss:// binds the connection.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
	// HubProofHeader carries the hub's MAC of the nonce on its 101.
	HubProofHeader = "Dax-Hub-Proof"
	// ProofChallenge is the hub's WWW-Authenticate on a 401.
	ProofChallenge = ProofScheme + ` realm="dax"`
	// ProofWindow is how far a nonce's time may be from the hub's
	// clock, either way, and so how long the hub remembers a nonce it
	// took: the two hosts' clocks may differ by this much.
	ProofWindow = 5 * time.Minute

	workspaceLabel = "dax-tunnel workspace v1"
	hubLabel       = "dax-tunnel hub v1"
)

// Errors of the hub's check; each is a refusal.
var (
	ErrNoProof       = errors.New("authn: the upgrade carries no tunnel proof")
	ErrBadProof      = errors.New("authn: the upgrade's proof is not made with a known token")
	ErrStaleNonce    = fmt.Errorf("authn: the upgrade's nonce is more than %s from this host's clock (do the two hosts' clocks agree?)", ProofWindow)
	ErrReplayedNonce = errors.New("authn: the upgrade's nonce was used before")
	// ErrHubProof is a hub whose answer does not prove it holds the
	// token.
	ErrHubProof = errors.New("authn: the hub's answer does not prove it holds the token")
)

// mac is the hex HMAC-SHA256 under key of label, nonce and bound, each
// ended by a zero byte: bound is what else the MAC covers (the run's
// instance id), so a relay cannot change it.
func mac(key []byte, label, nonce, bound string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(label + "\x00" + nonce + "\x00" + bound + "\x00"))
	return hex.EncodeToString(m.Sum(nil))
}

// newNonce is a nonce for an upgrade at now: its Unix time in seconds,
// a dot, and 26 random base32 characters.
func newNonce(now time.Time) string {
	return strconv.FormatInt(now.Unix(), 10) + "." + rand.Text()
}

// nonceTime is the time a nonce names, and whether it is one newNonce
// makes.
func nonceTime(nonce string) (time.Time, bool) {
	secs, random, ok := strings.Cut(nonce, ".")
	if !ok || len(secs) == 0 || len(secs) > 12 || len(random) != 26 {
		return time.Time{}, false
	}
	for _, c := range random {
		if (c < 'A' || c > 'Z') && (c < '2' || c > '7') {
			return time.Time{}, false
		}
	}
	n, err := strconv.ParseInt(secs, 10, 64)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != secs {
		return time.Time{}, false
	}
	return time.Unix(n, 0), true
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
	if !ok || len(proof) != 2*sha256.Size || strings.ContainsAny(nonce, `"`) {
		return "", "", false
	}
	return nonce, proof, true
}

// Prover is the workspace end's half: it makes each upgrade's proof
// and checks the hub's answer. The token is the key, never sent.
type Prover struct {
	key []byte
	now func() time.Time
}

// NewProver is the proof under token.
func NewProver(token string) *Prover {
	return &Prover{key: []byte(token), now: time.Now}
}

// Header is the header of one upgrade: a fresh nonce and the workspace
// end's MAC of it and bound, in Authorization.
func (p *Prover) Header(bound string) http.Header {
	nonce := newNonce(p.now())
	return http.Header{"Authorization": {fmt.Sprintf(`%s nonce="%s", proof="%s"`, ProofScheme, nonce, mac(p.key, workspaceLabel, nonce, bound))}}
}

// CheckHub reports whether resp, the answer to req (as sent, with
// Header's Authorization), carries the hub's MAC of req's nonce and
// bound under the token; ErrHubProof otherwise.
func (p *Prover) CheckHub(req *http.Request, resp *http.Response, bound string) error {
	nonce, _, ok := proofOf(req)
	if !ok {
		return fmt.Errorf("%w: the upgrade carried no nonce", ErrHubProof)
	}
	got := resp.Header.Get(HubProofHeader)
	if got == "" {
		return fmt.Errorf("%w: it sent none", ErrHubProof)
	}
	if !hmac.Equal([]byte(got), []byte(mac(p.key, hubLabel, nonce, bound))) {
		return ErrHubProof
	}
	return nil
}

// Proofs is the hub's half: the tokens it knows, each naming a
// principal, and the nonces it has taken within ProofWindow.
type Proofs[P any] struct {
	entries []proofEntry[P]
	now     func() time.Time

	mu   sync.Mutex
	seen map[string]time.Time // a nonce taken, and when it may be forgotten
}

type proofEntry[P any] struct {
	key []byte
	p   P
}

// NewProofs is the hub's check of tokens; an empty token is left out.
func NewProofs[P any](tokens map[string]P) *Proofs[P] {
	v := &Proofs[P]{now: time.Now, seen: map[string]time.Time{}}
	for tok, p := range tokens {
		if tok != "" {
			v.entries = append(v.entries, proofEntry[P]{[]byte(tok), p})
		}
	}
	return v
}

// verify is the entry whose token made r's proof of bound, the nonce,
// and whether the nonce's time is within the window; every entry is
// tried, so the time taken does not say which matched.
func (v *Proofs[P]) verify(r *http.Request, bound string) (proofEntry[P], string, error) {
	var found proofEntry[P]
	nonce, proof, ok := proofOf(r)
	if !ok {
		return found, "", ErrNoProof
	}
	at, ok := nonceTime(nonce)
	if !ok {
		return found, "", ErrNoProof
	}
	matched := false
	for _, e := range v.entries {
		if hmac.Equal([]byte(proof), []byte(mac(e.key, workspaceLabel, nonce, bound))) {
			found, matched = e, true
		}
	}
	if !matched {
		return found, "", ErrBadProof
	}
	if d := v.now().Sub(at); d > ProofWindow || d < -ProofWindow {
		return found, "", ErrStaleNonce
	}
	return found, nonce, nil
}

// Check names the principal whose token made r's proof of bound, with
// a nonce within the window, without taking the nonce: a look before
// the enrollment (Take) decides.
func (v *Proofs[P]) Check(r *http.Request, bound string) (P, error) {
	e, _, err := v.verify(r, bound)
	return e.p, err
}

// Take is Check, and takes the nonce: one taken before within the
// window is ErrReplayedNonce, so a proof is good for one upgrade.
func (v *Proofs[P]) Take(r *http.Request, bound string) (P, error) {
	e, nonce, err := v.verify(r, bound)
	if err != nil {
		var zero P
		return zero, err
	}
	now := v.now()
	v.mu.Lock()
	defer v.mu.Unlock()
	for n, until := range v.seen {
		if now.After(until) {
			delete(v.seen, n)
		}
	}
	if _, used := v.seen[nonce]; used {
		var zero P
		return zero, ErrReplayedNonce
	}
	// Remembered until its time is out of the window for good.
	at, _ := nonceTime(nonce)
	v.seen[nonce] = at.Add(ProofWindow + time.Second)
	return e.p, nil
}

// HubHeader is the hub's answer to r, a proof Take accepted: its MAC of
// r's nonce and bound under the token that made r's proof, in
// HubProofHeader.
func (v *Proofs[P]) HubHeader(r *http.Request, bound string) (http.Header, error) {
	nonce, proof, ok := proofOf(r)
	if !ok {
		return nil, ErrNoProof
	}
	var key []byte
	for _, e := range v.entries {
		if hmac.Equal([]byte(proof), []byte(mac(e.key, workspaceLabel, nonce, bound))) {
			key = e.key
		}
	}
	if key == nil {
		return nil, ErrBadProof
	}
	return http.Header{HubProofHeader: {mac(key, hubLabel, nonce, bound)}}, nil
}
