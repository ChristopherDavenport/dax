package provider

import (
	"net/http"

	"github.com/ChristopherDavenport/agentturn/session"
)

// headerTransport names, on every request, the session the request is
// made for and the client making it, under the header names the user
// gave: a server that groups a client's calls by session, or keys a
// prompt cache on it, reads them there. The session is the one the run
// making the request is written to, from its context, so a sub-agent's
// requests name the sub-agent's session and the main run's the main
// one; a request made outside a run, such as asking the vendor what a
// model supports, names none.
type headerTransport struct {
	session, client, clientValue string
	next                         http.RoundTripper
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	id := session.SessionIDFromContext(req.Context())
	if (t.session == "" || id == "") && t.client == "" {
		return t.next.RoundTrip(req)
	}
	// A RoundTripper must not change the request it was given.
	r := req.Clone(req.Context())
	if t.session != "" && id != "" {
		r.Header.Set(t.session, id)
	}
	if t.client != "" {
		r.Header.Set(t.client, t.clientValue)
	}
	return t.next.RoundTrip(r)
}

// transport is the round tripper a vendor's client sends through: next
// (nil is http.DefaultTransport) under the command's key when keys is
// set, put with set, and under spec's session and client headers when
// it names them. It is nil when there is nothing to add, so the client
// keeps its own.
func transport(spec Spec, keys *keySource, set func(http.Header, string), next http.RoundTripper) http.RoundTripper {
	if keys == nil && spec.SessionHeader == "" && spec.ClientHeader == "" {
		return nil
	}
	if next == nil {
		next = http.DefaultTransport
	}
	if keys != nil {
		next = keyTransport{keys: keys, set: set}.wrap(next)
	}
	if spec.SessionHeader != "" || spec.ClientHeader != "" {
		next = &headerTransport{session: spec.SessionHeader, client: spec.ClientHeader, clientValue: spec.Client, next: next}
	}
	return next
}
