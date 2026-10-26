package authn

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	proofToken = "the-tunnel-token-of-at-least-32-bytes"
	otherToken = "another-token-of-at-least-32-bytes-long"
	instance   = "0123456789abcdef0123456789abcdef"
)

// upgrade is a request carrying h.
func upgrade(h http.Header) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:7780/tunnel", nil)
	r.Header = h
	return r
}

// The wire: Authorization is `Dax-Tunnel nonce="<unix>.<26 base32>",
// proof="<hex HMAC-SHA256>"`, the MAC of "dax-tunnel workspace v1",
// the nonce and the bound, each ended by a zero byte, under the token;
// the hub's is the same under "dax-tunnel hub v1". The token itself is
// in neither.
func TestTheProofsWire(t *testing.T) {
	h := NewProver(proofToken).Header(instance)
	auth := h.Get("Authorization")
	if strings.Contains(auth, proofToken) {
		t.Fatalf("the upgrade carries the token: %q", auth)
	}
	nonce, proof, ok := proofOf(upgrade(h))
	if !ok {
		t.Fatalf("Authorization %q", auth)
	}
	if _, ok := nonceTime(nonce); !ok {
		t.Errorf("nonce %q", nonce)
	}
	m := hmac.New(sha256.New, []byte(proofToken))
	m.Write([]byte("dax-tunnel workspace v1\x00" + nonce + "\x00" + instance + "\x00"))
	if want := hex.EncodeToString(m.Sum(nil)); proof != want {
		t.Errorf("proof %s, want %s", proof, want)
	}
	hub, err := NewProofs(map[string]string{proofToken: "build"}).HubHeader(upgrade(h), instance)
	if err != nil {
		t.Fatal(err)
	}
	m = hmac.New(sha256.New, []byte(proofToken))
	m.Write([]byte("dax-tunnel hub v1\x00" + nonce + "\x00" + instance + "\x00"))
	if got, want := hub.Get(HubProofHeader), hex.EncodeToString(m.Sum(nil)); got != want {
		t.Errorf("the hub's proof %s, want %s", got, want)
	}
	if again := NewProver(proofToken).Header(instance).Get("Authorization"); again == auth {
		t.Error("two upgrades carry one nonce")
	}
}

// The hub takes a proof made with a known token, of the instance it
// carries, with a nonce within the window, once.
func TestTheHubTakesAProofOnce(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	at := func(d time.Duration) *Prover {
		return &Prover{key: []byte(proofToken), now: func() time.Time { return now.Add(d) }}
	}
	other := &Prover{key: []byte(otherToken), now: func() time.Time { return now }}
	for _, tc := range []struct {
		name  string
		h     http.Header
		bound string
		want  error
	}{
		{"fresh", at(0).Header(instance), instance, nil},
		{"within the window ahead", at(ProofWindow - time.Second).Header(instance), instance, nil},
		{"within the window behind", at(-ProofWindow + time.Second).Header(instance), instance, nil},
		{"too old", at(-ProofWindow - time.Second).Header(instance), instance, ErrStaleNonce},
		{"too far ahead", at(ProofWindow + time.Second).Header(instance), instance, ErrStaleNonce},
		{"another token", other.Header(instance), instance, ErrBadProof},
		{"another instance", at(0).Header(instance), strings.Repeat("f", 32), ErrBadProof},
		{"the token as a bearer", http.Header{"Authorization": {"Bearer " + proofToken}}, instance, ErrNoProof},
		{"nothing", http.Header{}, instance, ErrNoProof},
		{"a nonce not as made", http.Header{"Authorization": {`Dax-Tunnel nonce="x", proof="` + strings.Repeat("0", 64) + `"`}}, instance, ErrNoProof},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := NewProofs(map[string]string{proofToken: "build", otherToken + "x": "else"})
			v.now = func() time.Time { return now }
			if _, err := v.Check(upgrade(tc.h), tc.bound); !errors.Is(err, tc.want) {
				t.Errorf("Check: %v, want %v", err, tc.want)
			}
			name, err := v.Take(upgrade(tc.h), tc.bound)
			if !errors.Is(err, tc.want) || err == nil && name != "build" {
				t.Fatalf("Take: %q, %v, want %v", name, err, tc.want)
			}
			if err != nil {
				return
			}
			// Replayed: the same nonce and MAC again.
			if _, err := v.Take(upgrade(tc.h), tc.bound); !errors.Is(err, ErrReplayedNonce) {
				t.Errorf("a replay: %v, want ErrReplayedNonce", err)
			}
			// Forgotten only once it is stale for good.
			v.now = func() time.Time { return now.Add(3 * ProofWindow) }
			if _, err := v.Take(upgrade(tc.h), tc.bound); !errors.Is(err, ErrStaleNonce) {
				t.Errorf("a replay later: %v, want ErrStaleNonce", err)
			}
			if len(v.seen) != 1 {
				t.Errorf("%d nonces remembered", len(v.seen))
			}
		})
	}
}

// The workspace end takes the hub's answer only when it is the hub's
// MAC of the nonce it sent, under its token: not none, not its own MAC
// sent back, not one under another token or of another instance.
func TestTheWorkspaceChecksTheHub(t *testing.T) {
	p := NewProver(proofToken)
	req := upgrade(p.Header(instance))
	nonce, mine, _ := proofOf(req)
	hubMAC := func(key, bound string) string { return mac([]byte(key), hubLabel, nonce, bound) }
	for _, tc := range []struct {
		name, answer string
		ok           bool
	}{
		{"the hub's", hubMAC(proofToken, instance), true},
		{"none", "", false},
		{"its own sent back", mine, false},
		{"under another token", hubMAC(otherToken, instance), false},
		{"of another instance", hubMAC(proofToken, strings.Repeat("f", 32)), false},
	} {
		resp := &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: http.Header{}}
		if tc.answer != "" {
			resp.Header.Set(HubProofHeader, tc.answer)
		}
		err := p.CheckHub(req, resp, instance)
		if (err == nil) != tc.ok || err != nil && !errors.Is(err, ErrHubProof) {
			t.Errorf("%s: %v", tc.name, err)
		}
		if err != nil && strings.Contains(err.Error(), proofToken) {
			t.Errorf("%s: the error holds the token", tc.name)
		}
	}
}
