package a2a

import "net/http"

// WithTransport is o with its clients' requests carried by rt, to trust
// a test server's certificate.
func (o Options) WithTransport(rt http.RoundTripper) Options {
	o.transport = rt
	return o
}
