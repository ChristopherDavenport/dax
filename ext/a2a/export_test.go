package a2a

import "net/http"

// SetTransport makes the clients dax-a2a builds use rt, to trust a test
// server's certificate, until the returned func is called.
func SetTransport(rt http.RoundTripper) (restore func()) {
	testTransport = rt
	return func() { testTransport = nil }
}
