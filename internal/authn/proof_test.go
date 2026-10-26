package authn

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
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

// clock is a hub whose age is set by the test.
func clock(h *Hub) *time.Duration {
	var age time.Duration
	h.since = func() time.Duration { return age }
	return &age
}

// The wire: the hub's nonce is "<age ns>.<26 base32>.<32 hex>", the
// last its MAC of the rest under a secret of its own; Authorization is
// `Dax-Tunnel nonce="<nonce>", proof="<hex HMAC-SHA256>"`, the MAC of
// "dax-tunnel workspace v2", the name, the nonce and the bound, each
// ended by a zero byte, under the token; the hub's answer is the same
// under "dax-tunnel hub v2". The token is in none of them.
func TestTheProofsWire(t *testing.T) {
	hub := NewHub(proofToken, "build")
	nonce := hub.Nonce()
	parts := strings.Split(nonce, ".")
	if len(parts) != 3 || len(parts[1]) != 26 || len(parts[2]) != 32 || !validNonce(nonce) {
		t.Fatalf("nonce %q", nonce)
	}
	if _, err := strconv.ParseInt(parts[0], 10, 64); err != nil {
		t.Errorf("nonce %q: %v", nonce, err)
	}
	if strings.Contains(nonce, proofToken) || hub.Nonce() == nonce {
		t.Errorf("nonce %q", nonce)
	}
	h := NewProver(proofToken, "build").Header(nonce, instance)
	auth := h.Get("Authorization")
	if strings.Contains(auth, proofToken) {
		t.Fatalf("the upgrade carries the token: %q", auth)
	}
	got, proof, ok := proofOf(upgrade(h))
	if !ok || got != nonce {
		t.Fatalf("Authorization %q", auth)
	}
	hm := func(label string) string {
		m := hmac.New(sha256.New, []byte(proofToken))
		m.Write([]byte(label + "\x00build\x00" + nonce + "\x00" + instance + "\x00"))
		return hex.EncodeToString(m.Sum(nil))
	}
	if want := hm("dax-tunnel workspace v2"); proof != want {
		t.Errorf("proof %s, want %s", proof, want)
	}
	answer, err := hub.Header(upgrade(h), instance)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := answer.Get(HubProofHeader), hm("dax-tunnel hub v2"); got != want {
		t.Errorf("the hub's proof %s, want %s", got, want)
	}
}

// The hub takes a proof made with its token and name, of the instance
// it carries, over a nonce it issued within NonceLifetime, once.
func TestTheHubTakesAProofOnce(t *testing.T) {
	impostor := NewHub(proofToken, "build") // another hub: its own secret
	for _, tc := range []struct {
		name  string
		h     func(nonce string) http.Header
		age   time.Duration // the hub's age at the upgrade, issued at 1h
		bound string
		want  error
	}{
		{"fresh", func(n string) http.Header { return NewProver(proofToken, "build").Header(n, instance) }, time.Hour, instance, nil},
		{"at the end of its life", func(n string) http.Header { return NewProver(proofToken, "build").Header(n, instance) }, time.Hour + NonceLifetime, instance, nil},
		{"too old", func(n string) http.Header { return NewProver(proofToken, "build").Header(n, instance) }, time.Hour + NonceLifetime + time.Nanosecond, instance, ErrStaleNonce},
		{"issued later than now", func(n string) http.Header { return NewProver(proofToken, "build").Header(n, instance) }, time.Hour - time.Second, instance, ErrStaleNonce},
		{"another hub's nonce", func(string) http.Header {
			return NewProver(proofToken, "build").Header(impostor.Nonce(), instance)
		}, time.Hour, instance, ErrUnknownNonce},
		{"a nonce made up", func(string) http.Header {
			return NewProver(proofToken, "build").Header("1.AAAAAAAAAAAAAAAAAAAAAAAAAA.0123", instance)
		}, time.Hour, instance, ErrUnknownNonce},
		{"another token", func(n string) http.Header { return NewProver(otherToken, "build").Header(n, instance) }, time.Hour, instance, ErrBadProof},
		{"another name", func(n string) http.Header { return NewProver(proofToken, "other").Header(n, instance) }, time.Hour, instance, ErrBadProof},
		{"another instance", func(n string) http.Header { return NewProver(proofToken, "build").Header(n, instance) }, time.Hour, strings.Repeat("f", 32), ErrBadProof},
		{"the token as a bearer", func(string) http.Header { return http.Header{"Authorization": {"Bearer " + proofToken}} }, time.Hour, instance, ErrNoProof},
		{"nothing", func(string) http.Header { return http.Header{} }, time.Hour, instance, ErrNoProof},
		{"a nonce with a quote", func(string) http.Header {
			return http.Header{"Authorization": {`Dax-Tunnel nonce="a"b", proof="` + strings.Repeat("0", 64) + `"`}}
		}, time.Hour, instance, ErrNoProof},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := NewHub(proofToken, "build")
			age := clock(hub)
			*age = time.Hour
			h := tc.h(hub.Nonce())
			*age = tc.age
			if err := hub.Check(upgrade(h), tc.bound); !errors.Is(err, tc.want) {
				t.Errorf("Check: %v, want %v", err, tc.want)
			}
			err := hub.Take(upgrade(h), tc.bound)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Take: %v, want %v", err, tc.want)
			}
			if _, err := hub.Header(upgrade(h), tc.bound); !errors.Is(err, tc.want) {
				t.Errorf("Header: %v, want %v", err, tc.want)
			}
			if err != nil {
				if len(hub.taken) != 0 {
					t.Errorf("a refused nonce was kept: %v", hub.taken)
				}
				return
			}
			if err := hub.Take(upgrade(h), tc.bound); !errors.Is(err, ErrReplayedNonce) {
				t.Errorf("a replay: %v, want ErrReplayedNonce", err)
			}
		})
	}
}

// Taken nonces are dropped once they expire, oldest first, and no more
// than MaxLiveNonces are held: past that an upgrade is ErrBusy.
func TestTheHubsTakenNoncesAreBounded(t *testing.T) {
	hub := NewHub(proofToken, "build")
	age := clock(hub)
	p := NewProver(proofToken, "build")
	take := func() error {
		return hub.Take(upgrade(p.Header(hub.Nonce(), instance)), instance)
	}
	for i := range MaxLiveNonces {
		if err := take(); err != nil {
			t.Fatalf("take %d: %v", i, err)
		}
	}
	if err := take(); !errors.Is(err, ErrBusy) {
		t.Fatalf("one past the bound: %v", err)
	}
	*age += NonceLifetime + time.Nanosecond
	if err := take(); err != nil {
		t.Fatalf("after the taken expired: %v", err)
	}
	if len(hub.taken) != 1 || len(hub.order) != 1 {
		t.Errorf("%d taken, %d in order, want 1 each", len(hub.taken), len(hub.order))
	}
}

// The workspace end takes the hub's answer only when it is the hub's
// MAC of the name, the nonce it sent and its instance, under its token:
// not none, not its own MAC sent back, not one under another token, of
// another name or of another instance.
func TestTheWorkspaceChecksTheHub(t *testing.T) {
	hub := NewHub(proofToken, "build")
	p := NewProver(proofToken, "build")
	req := upgrade(p.Header(hub.Nonce(), instance))
	nonce, mine, _ := proofOf(req)
	hubMAC := func(key, name, bound string) string { return mac([]byte(key), hubLabel, name, nonce, bound) }
	for _, tc := range []struct {
		name, answer string
		ok           bool
	}{
		{"the hub's", hubMAC(proofToken, "build", instance), true},
		{"none", "", false},
		{"its own sent back", mine, false},
		{"under another token", hubMAC(otherToken, "build", instance), false},
		{"of another name", hubMAC(proofToken, "other", instance), false},
		{"of another instance", hubMAC(proofToken, "build", strings.Repeat("f", 32)), false},
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

// status answers code.
func status(code int) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) { w.WriteHeader(code) }
}

// FetchNonce asks the hub's URL over http for its nonce. An answer that
// rules a session's hub out is ErrNoNonce; one a hub may give for a
// while is ErrNonceLater, to try again on.
func TestFetchNonce(t *testing.T) {
	hub := NewHub(proofToken, "build")
	for _, tc := range []struct {
		name   string
		answer func(w http.ResponseWriter)
		want   error
	}{
		{"a nonce", func(w http.ResponseWriter) { w.Header().Set(NonceHeader, hub.Nonce()) }, nil},
		{"none", func(http.ResponseWriter) {}, ErrNoNonce},
		{"not a nonce", func(w http.ResponseWriter) { w.Header().Set(NonceHeader, `a"b`) }, ErrNoNonce},
		{"an older dax hub's 401", status(http.StatusUnauthorized), ErrNoNonce},
		{"403", status(http.StatusForbidden), ErrNoNonce},
		{"404", status(http.StatusNotFound), ErrNoNonce},
		{"a redirect", func(w http.ResponseWriter) {
			w.Header().Set("Location", "https://elsewhere.example/")
			w.WriteHeader(http.StatusFound)
		}, ErrNoNonce},
		{"503, for a while", status(http.StatusServiceUnavailable), ErrNonceLater},
		{"502", status(http.StatusBadGateway), ErrNonceLater},
		{"429", status(http.StatusTooManyRequests), ErrNonceLater},
		{"408", status(http.StatusRequestTimeout), ErrNonceLater},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/tunnel" || r.Header.Get("Upgrade") != "" {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				tc.answer(w)
			}))
			defer srv.Close()
			hc := srv.Client()
			hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			nonce, err := FetchNonce(t.Context(), hc, "ws"+strings.TrimPrefix(srv.URL, "http")+"/tunnel")
			if !errors.Is(err, tc.want) || err == nil && !validNonce(nonce) {
				t.Errorf("%q, %v; want %v", nonce, err, tc.want)
			}
		})
	}
}

// A nonce's age starts at a random offset, so it does not say when the
// hub started.
func TestANonceDoesNotTellTheHubsStart(t *testing.T) {
	ages := map[int64]bool{}
	for range 4 {
		age, err := strconv.ParseInt(strings.Split(NewHub(proofToken, "build").Nonce(), ".")[0], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		if age < int64(time.Second) {
			t.Errorf("a new hub's nonce says it is %v old", time.Duration(age))
		}
		ages[age/int64(time.Second)] = true
	}
	if len(ages) < 2 {
		t.Errorf("four new hubs' nonces give one age: %v", ages)
	}
}
