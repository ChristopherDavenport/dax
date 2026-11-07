package agent

// An MCP server at a URL: streamable HTTP, or HTTP+SSE for a server
// that refuses it, called from this machine whatever the workspace,
// with a token sent to the URL's origin alone.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ChristopherDavenport/dax/internal/config"
)

// mcpURL is the dial of the server at m.URL, called from this machine:
// streamable HTTP, or HTTP+SSE for a server that refuses it.
//
// When m.TokenEnv is set, the token it holds is sent as a bearer token
// on every request to the URL's origin, and on none to any other: a
// request elsewhere is refused, and so is a redirect off the origin,
// since Go's client keeps the Authorization header on a redirect to
// another port or from https to http. A variable that is not set is an
// error here, before anything is sent. With m.OAuth, the token is the
// grant the person signs in for (mcpoauth.go), held to the same origin.
//
// The first dial asks the server whether it speaks streamable HTTP: a
// POST of initialize, as the MCP transports' backwards compatibility
// has a client do. A 4xx (but an authorization's 401 or 403, a 408 or a
// 429) followed by a GET that opens an SSE stream whose first event is
// "endpoint" is a server of the older HTTP+SSE transport, dialled over
// SSEClientTransport from then on, which the user is told once; any
// other answer is streamable HTTP's, and a session the probe opened is
// ended. Each dial is a new transport.
func (s *Session) mcpURL(ctx context.Context, m MCPServer) (mcpDial, error) {
	endpoint := m.URL
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("url %q: not a URL", endpoint)
	}
	g := &originGuard{origin: originOf(u), rt: http.DefaultTransport}
	if m.TokenEnv != "" {
		if g.token = os.Getenv(m.TokenEnv); g.token == "" {
			return nil, fmt.Errorf("token_env %s is not set; the server at %s is not reached without its token", m.TokenEnv, g.origin)
		}
	}
	client := &http.Client{CheckRedirect: g.redirect, Transport: g}
	var oh *auth.AuthorizationCodeHandler
	if m.OAuth != nil {
		if m.TokenEnv != "" {
			return nil, errors.New("token_env and oauth: give one")
		}
		if oh, err = s.oauthHandler(ctx, m, g.origin); err != nil {
			return nil, err
		}
	}
	// The SSE transport has no OAuth handler of its own: its client
	// sends the grant's current token, to the origin alone.
	sg := &originGuard{origin: g.origin, token: g.token, rt: g.rt}
	if oh != nil {
		sg.source = func(ctx context.Context) string {
			ts, err := oh.TokenSource(ctx)
			if err != nil || ts == nil {
				return ""
			}
			tok, err := ts.Token()
			if err != nil {
				return ""
			}
			return tok.AccessToken
		}
	}
	sseClient := &http.Client{CheckRedirect: sg.redirect, Transport: sg}
	var once sync.Once
	var legacy bool
	return func(ctx context.Context) (mcp.Transport, error) {
		once.Do(func() {
			if code := refusesStreamable(ctx, client, endpoint); code != 0 {
				legacy = true
				fmt.Fprintf(warnings{}, "dax: mcp %s: the server refused streamable HTTP (%d %s); it is reached over HTTP+SSE\n", m.Name, code, http.StatusText(code))
			}
		})
		if legacy {
			return &mcp.SSEClientTransport{Endpoint: endpoint, HTTPClient: sseClient}, nil
		}
		t := &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: client}
		if oh != nil {
			t.OAuthHandler = oh
		}
		return t, nil
	}, nil
}

// refusesStreamable POSTs an initialize to endpoint and returns the
// status of a 4xx that says the server does not speak streamable HTTP,
// 0 for any other answer. A 4xx says so only when a GET of endpoint
// then opens an SSE stream whose first event is "endpoint", the old
// transport's own greeting: a streamable server answering 400 for a
// missing credential is not moved. A session the POST opened is ended.
func refusesStreamable(ctx context.Context, client *http.Client, endpoint string) int {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	body := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"dax","version":"` + Version + `"}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return 0
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	switch code := resp.StatusCode; {
	case code >= 200 && code < 300:
		if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
			if del, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil); err == nil {
				del.Header.Set("Mcp-Session-Id", id)
				if r, err := client.Do(del); err == nil {
					r.Body.Close()
				}
			}
		}
	case code >= 400 && code < 500 && code != http.StatusUnauthorized && code != http.StatusForbidden && code != http.StatusRequestTimeout && code != http.StatusTooManyRequests:
		if greetsOverSSE(ctx, client, endpoint) {
			return code
		}
	}
	return 0
}

// greetsOverSSE says whether a GET of endpoint opens an SSE stream whose
// first event is "endpoint", within ten seconds. The stream is closed.
func greetsOverSSE(ctx context.Context, client *http.Client, endpoint string) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return false
	}
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 64<<10))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || strings.HasPrefix(line, ":"):
			continue
		case strings.HasPrefix(line, "event:"):
			return strings.TrimSpace(strings.TrimPrefix(line, "event:")) == "endpoint"
		default:
			// Data before any event name is a "message" event.
			return false
		}
	}
	return false
}

// originGuard holds an MCP server's requests to its origin and adds the
// token to them: the static one, or source's.
type originGuard struct {
	origin, token string
	source        func(context.Context) string
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
	token := g.token
	if g.source != nil {
		token = g.source(req.Context())
	}
	if token != "" {
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+token)
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
