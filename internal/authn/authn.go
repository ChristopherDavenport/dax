// Package authn is dax's own check of who sends a request, and the
// header its clients send: the libraries dax serves and dials through
// (agentworkspace's remote, agentsession's remote, agentturn's
// front/control and front/a2a) take an authenticator and an HTTP
// client, and ship no authentication of their own that dax relies on.
//
// Tokens is a set of static bearer tokens, each naming a principal of
// the library's own type. Its Authenticator is a func of the shape
// every library's hook takes, so it is passed to WithAuthenticator as
// it is. Tokens are compared with every known one, as SHA-256 hashes
// and in constant time, so neither which token matched nor its length
// shows in the time a request takes; an empty token is never known and
// never accepted. Client is an http.Client that sends one token as
// "Authorization: Bearer <token>" to one origin and nowhere else, which
// a library's client takes through its WithHTTPClient.
//
// Prover and Hub are the tunnel's proof (proof.go): the workspace
// end and the hub each show the other they hold the tunnel's token,
// an HMAC under it of a nonce the hub issued, and neither sends the
// token.
//
// It imports the standard library alone.
package authn

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// Challenge is the WWW-Authenticate of every 401 a handler dax mounts
// behind Tokens writes (each library's WithChallenge): the scheme and
// realm a client is to answer with.
const Challenge = `Bearer realm="dax"`

// Tokens is a set of static bearer tokens and the principal each names.
type Tokens[P any] struct {
	entries []entry[P]
}

type entry[P any] struct {
	sum [sha256.Size]byte
	p   P
}

// New is the set tokens names; an empty token is left out.
func New[P any](tokens map[string]P) *Tokens[P] {
	t := &Tokens[P]{}
	for tok, p := range tokens {
		if tok == "" {
			continue
		}
		t.entries = append(t.entries, entry[P]{sha256.Sum256([]byte(tok)), p})
	}
	return t
}

// lookup is the principal tok names: every entry is compared, so the
// time taken is the same whichever matched, or none.
func (t *Tokens[P]) lookup(tok string) (P, bool) {
	var found P
	if tok == "" {
		return found, false
	}
	sum := sha256.Sum256([]byte(tok))
	matched := 0
	for _, e := range t.entries {
		if subtle.ConstantTimeCompare(sum[:], e.sum[:]) == 1 {
			found, matched = e.p, 1
		}
	}
	if matched == 0 {
		var zero P
		return zero, false
	}
	return found, true
}

// Authenticator accepts a request whose Authorization header is
// exactly "Bearer <token>" for a known token, and names its principal;
// any other request is refused with unauthenticated, the library's own
// error. Its type is the hook agentworkspace's and agentsession's
// remote and agentturn's front/control take.
func (t *Tokens[P]) Authenticator(unauthenticated error) func(*http.Request) (P, error) {
	return func(r *http.Request) (P, error) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if p, known := t.lookup(tok); ok && known {
			return p, nil
		}
		var zero P
		return zero, unauthenticated
	}
}

// ErrNoToken is a request that carries no known bearer token.
var ErrNoToken = errors.New("authn: no known bearer token")

// Principal accepts a request whose Authorization header is "Bearer"
// in any case, a space and a known token, space around it trimmed, and
// names its principal; any other is ErrNoToken. It is A2A's reading of
// the scheme, the hook front/a2a's Authenticator takes as Principal.
func (t *Tokens[P]) Principal(r *http.Request) (P, error) {
	scheme, tok, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if strings.EqualFold(scheme, "Bearer") {
		if p, ok := t.lookup(strings.TrimSpace(tok)); ok {
			return p, nil
		}
	}
	var zero P
	return zero, ErrNoToken
}

// Client is base (http.DefaultClient's settings when nil), its
// redirects and timeout kept, sending token as a bearer token on every
// request to the origin of baseURL, its scheme and host, and on none
// to any other, so a redirect or a URL elsewhere does not carry it.
func Client(base *http.Client, baseURL, token string) (*http.Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		// url's error quotes the URL; the caller says which.
		return nil, errors.New("authn: the URL has no origin")
	}
	c := &http.Client{}
	if base != nil {
		*c = *base
	}
	c.Transport = &Bearer{Token: token, Scheme: u.Scheme, Host: u.Host, Base: c.Transport}
	return c, nil
}

// Bearer is an http.RoundTripper that adds "Authorization: Bearer
// <Token>" to a request for Scheme://Host, and passes every request on
// to Base (http.DefaultTransport when nil).
type Bearer struct {
	Token        string
	Scheme, Host string
	Base         http.RoundTripper
}

// RoundTrip sends r, with the token when it is for the origin.
func (b *Bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	base := b.Base
	if base == nil {
		base = http.DefaultTransport
	}
	if b.Token == "" || !strings.EqualFold(r.URL.Scheme, b.Scheme) || !strings.EqualFold(r.URL.Host, b.Host) {
		return base.RoundTrip(r)
	}
	// A RoundTripper does not change the request it is given.
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.Token)
	return base.RoundTrip(r)
}
