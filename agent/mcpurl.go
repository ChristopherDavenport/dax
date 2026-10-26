package agent

// An MCP server at a URL: streamable HTTP, called from this machine
// whatever the workspace, with a token sent to the URL's origin alone.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ChristopherDavenport/dax/internal/config"
)

// mcpURL is the dial of the streamable HTTP server at endpoint. When
// tokenEnv is set, the token it holds is sent as a bearer token on every
// request to endpoint's origin, and on none to any other: a request
// elsewhere is refused, and so is a redirect off the origin, since Go's
// client keeps the Authorization header on a redirect to another port
// or from https to http. A variable that is not set is an error here,
// before anything is sent. Each dial is a new transport.
func mcpURL(endpoint, tokenEnv string) (mcpDial, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("url %q: not a URL", endpoint)
	}
	g := &originGuard{origin: originOf(u), rt: http.DefaultTransport}
	if tokenEnv != "" {
		if g.token = os.Getenv(tokenEnv); g.token == "" {
			return nil, fmt.Errorf("token_env %s is not set; the server at %s is not reached without its token", tokenEnv, g.origin)
		}
	}
	client := &http.Client{CheckRedirect: g.redirect, Transport: g}
	return func(context.Context) (mcp.Transport, error) {
		return &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: client}, nil
	}, nil
}

// originGuard holds an MCP server's requests to its origin and adds the
// token to them.
type originGuard struct {
	origin, token string
	rt            http.RoundTripper
}

// RoundTrip sends req, which must be to the guard's origin, with the
// token when there is one.
func (g *originGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	if o := originOf(req.URL); o != g.origin {
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, fmt.Errorf("a request to %s, another origin than the server's %s, is not sent", o, g.origin)
	}
	if g.token != "" {
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	return g.rt.RoundTrip(req)
}

// redirect refuses a redirect to another origin.
func (g *originGuard) redirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if o := originOf(req.URL); o != g.origin {
		return fmt.Errorf("a redirect to %s, another origin than %s, is not followed", o, g.origin)
	}
	return nil
}

// originOf is u's scheme, host and port, the port filled in.
func originOf(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	return u.Scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// urlPlace is how Info.MCP names a URL's server: its origin, the
// scheme and host with no default port, as agentkit labels one, and
// never its user, path, query or fragment, where a hosted server's key
// is often put. The listing is shown to every view attached.
func urlPlace(endpoint string) string {
	u, err := url.Parse(config.WorkspaceRef(endpoint))
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
