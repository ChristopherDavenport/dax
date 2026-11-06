package agent

// A URL server that signs in with OAuth (#175): the MCP authorization
// spec's flow through go-sdk's AuthorizationCodeHandler, the grant kept
// by agenttool's mcpclient.StoreTokens in a private file under ~/.dax,
// and dax's own guards on where each request may go. The library is the
// capability; who may sign in, where the grant is kept and what is sent
// where are dax's.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agenttool/mcpclient"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/ChristopherDavenport/dax/internal/private"
)

// OAuth is how a URL server's user signs in, when the server asks for
// OAuth (RFC 9728 discovery from its 401): with no client ID, dax
// registers itself with the authorization server (RFC 7591); with one,
// it is the client registered for dax there, its secret in
// ClientSecretEnv (a variable every child loses), and its redirect on
// CallbackPort, which such a registration names. The person consents
// in a browser at the URL dax prints, and the authorization server
// redirects to a loopback port on this machine. The grant is kept in
// ~/.dax/mcp-tokens.json, private, by the server's URL and the user, and
// refreshed from there by later sessions; it is never sent to a
// workspace.
type OAuth struct {
	ClientID        string
	ClientSecretEnv string
	CallbackPort    int
}

// TokensFile is the file under the user's dax directory that keeps the
// URL servers' OAuth grants.
func TokensFile(userDir string) string { return filepath.Join(userDir, "mcp-tokens.json") }

// consentOut is where the consent URL is printed: the terminal dax was
// started from, before any front takes the screen. Tests swap it.
var (
	consentMu  sync.Mutex
	consentOut io.Writer = os.Stderr
)

// swapConsentOut sends the consent line to w until what it returns is
// called; for tests.
func swapConsentOut(w io.Writer) func() {
	consentMu.Lock()
	old := consentOut
	consentOut = w
	consentMu.Unlock()
	return func() {
		consentMu.Lock()
		consentOut = old
		consentMu.Unlock()
	}
}

// consentTimeout is how long dax waits for the browser's redirect.
const consentTimeout = 5 * time.Minute

// oauthHandler is the authorization-code handler of the URL server m,
// its requests held by an authGuard to the server's origin and the
// authorization servers the server's metadata names, its grant kept in
// the tokens file.
func (s *Session) oauthHandler(ctx context.Context, m MCPServer, origin string) (*auth.AuthorizationCodeHandler, error) {
	o := m.OAuth
	port := o.CallbackPort
	if port == 0 {
		// A port free now, which a dynamic registration names; the
		// listener is opened on it when consent is asked for.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("oauth: a loopback port for the redirect: %w", err)
		}
		port = ln.Addr().(*net.TCPAddr).Port
		ln.Close()
	}
	redirect := "http://127.0.0.1:" + strconv.Itoa(port) + "/callback"
	g := newAuthGuard(origin, oauthBase)
	cfg := &auth.AuthorizationCodeHandlerConfig{
		RedirectURL:              redirect,
		AuthorizationCodeFetcher: s.consent(m.Name, port),
		RequestRefreshToken:      true,
		Client:                   &http.Client{Transport: g, CheckRedirect: g.redirect, Timeout: 30 * time.Second},
	}
	if o.ClientID != "" {
		cred := &oauthex.ClientCredentials{ClientID: o.ClientID}
		if o.ClientSecretEnv != "" {
			secret := os.Getenv(o.ClientSecretEnv)
			if secret == "" {
				return nil, fmt.Errorf("oauth: client_secret_env %s is not set", o.ClientSecretEnv)
			}
			cred.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: secret}
		}
		cfg.PreregisteredClient = cred
	} else {
		cfg.DynamicClientRegistrationConfig = &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
			RedirectURIs: []string{redirect}, ClientName: s.opts.Name,
			GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"},
			TokenEndpointAuthMethod: "none",
		}}
	}
	store := s.tokenStore()
	key := mcpclient.TokenKey{Endpoint: m.URL, Subject: tokenSubject()}
	// A kept grant is refreshed at the token endpoint it was granted by,
	// one the server's metadata named then, before any discovery now.
	if rec, err := store.Load(ctx, key); err == nil && rec != nil && rec.TokenURL != "" {
		if u, err := url.Parse(rec.TokenURL); err == nil && u.Host != "" {
			g.name(originOf(u))
		}
	}
	onSave := func(err error) {
		fmt.Fprintf(warnings{}, "dax: mcp %s: keeping its sign-in: %v\n", m.Name, err)
	}
	if err := mcpclient.StoreTokens(ctx, cfg, store, key, onSave); err != nil {
		return nil, fmt.Errorf("oauth: %w", err)
	}
	return auth.NewAuthorizationCodeHandler(cfg)
}

// oauthBase is the transport under the authorization client; tests
// swap it.
var oauthBase http.RoundTripper = http.DefaultTransport

// tokenStore is the grants' store: the private file under the user's
// directory, or, with none (a bare run), memory that ends with the
// session.
func (s *Session) tokenStore() mcpclient.TokenStore {
	if s.opts.UserDir == "" || s.opts.Bare != nil {
		return &mcpclient.MemoryTokenStore{}
	}
	return &fileTokens{path: TokensFile(s.opts.UserDir)}
}

// tokenSubject names the user a grant is kept for.
func tokenSubject() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}

// consent is the fetcher that asks the person to sign in to the server
// called name: it prints the authorization URL and takes the code from
// the browser's redirect to the loopback port. It is asked only while
// the session opens at the terminal dax was started from; later (a
// server started again mid-session, a /new or /resume while a front has
// the screen) the sign-in fails with what to do.
func (s *Session) consent(name string, port int) auth.AuthorizationCodeFetcher {
	return func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		if s.opened.Load() || s.opts.noConsent {
			return nil, fmt.Errorf("mcp %s needs you to sign in again, which dax asks only as a session starts at your terminal; start dax again", name)
		}
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			return nil, fmt.Errorf("mcp %s: the redirect's port %d: %w", name, port, err)
		}
		type got struct {
			res *auth.AuthorizationResult
			err error
		}
		done := make(chan got, 1)
		srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/callback" {
				http.NotFound(w, r)
				return
			}
			q := r.URL.Query()
			var g got
			if e := q.Get("error"); e != "" {
				g.err = fmt.Errorf("mcp %s: the authorization server refused: %s %s", name, e, q.Get("error_description"))
				fmt.Fprintln(w, "Sign-in refused; you can close this page.")
			} else {
				g.res = &auth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}
				fmt.Fprintln(w, "Signed in; you can close this page and go back to dax.")
			}
			select {
			case done <- g:
			default:
			}
		})}
		go srv.Serve(ln)
		defer srv.Close()
		consentMu.Lock()
		fmt.Fprintf(consentOut, "dax: mcp %s asks you to sign in. Open this URL in a browser:\n  %s\n(dax waits for its redirect to %s)\n", name, args.URL, "http://127.0.0.1:"+strconv.Itoa(port)+"/callback")
		consentMu.Unlock()
		wait, cancel := context.WithTimeout(ctx, consentTimeout)
		defer cancel()
		select {
		case g := <-done:
			return g.res, g.err
		case <-wait.Done():
			return nil, fmt.Errorf("mcp %s: no sign-in within %s", name, consentTimeout)
		}
	}
}

// authGuard holds the authorization client's requests to the MCP
// server's origin, for its protected-resource metadata, and to the
// origins of the authorization servers that metadata names, learnt from
// the metadata as it passes; any other request, and a redirect off
// those origins, is refused. It adds nothing: the MCP access token is
// never on these requests.
type authGuard struct {
	server string
	rt     http.RoundTripper
	mu     sync.Mutex
	named  map[string]bool
}

func newAuthGuard(serverOrigin string, rt http.RoundTripper) *authGuard {
	return &authGuard{server: serverOrigin, rt: rt, named: map[string]bool{}}
}

// name allows the origin o, an authorization server's.
func (g *authGuard) name(o string) {
	g.mu.Lock()
	g.named[o] = true
	g.mu.Unlock()
}

func (g *authGuard) allowed(o string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return o == g.server || g.named[o]
}

// RoundTrip sends req when its origin is allowed, and learns the
// authorization servers from a protected-resource metadata document the
// server's origin answers.
func (g *authGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	o := originOf(req.URL)
	if !g.allowed(o) {
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, fmt.Errorf("a sign-in request to %s, which is neither the server's origin nor an authorization server its metadata names, is not sent", o)
	}
	resp, err := g.rt.RoundTrip(req)
	if err != nil || o != g.server || resp.StatusCode != http.StatusOK || !strings.HasPrefix(req.URL.Path, "/.well-known/oauth-protected-resource") {
		return resp, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	var prm struct {
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if json.Unmarshal(body, &prm) == nil {
		g.mu.Lock()
		for _, a := range prm.AuthorizationServers {
			if u, err := url.Parse(a); err == nil && u.Host != "" && (u.Scheme == "https" || u.Scheme == "http" && loopbackHost(u.Hostname())) {
				g.named[originOf(u)] = true
			}
		}
		g.mu.Unlock()
	}
	return resp, nil
}

// redirect refuses a redirect off the allowed origins.
func (g *authGuard) redirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if o := originOf(req.URL); !g.allowed(o) {
		return fmt.Errorf("a sign-in redirect to %s, off the server's and its authorization servers' origins, is not followed", o)
	}
	return nil
}

// loopbackHost says whether host is this machine's loopback.
func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// fileTokens is the grants file: private (0600 in a 0700 directory),
// read and replaced whole on each save, its grants by server URL and
// user.
type fileTokens struct {
	path string
	mu   sync.Mutex
}

type tokensFileData struct {
	Grants map[string]*mcpclient.TokenRecord `json:"grants"`
}

func tokensKey(k mcpclient.TokenKey) string { return k.Endpoint + " " + k.Subject }

func (f *fileTokens) read() (tokensFileData, error) {
	var d tokensFileData
	if err := checkPrivate(f.path); err != nil {
		return d, fmt.Errorf("the MCP sign-in file %s: %w", f.path, err)
	}
	if fi, err := os.Stat(f.path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		return d, fmt.Errorf("the MCP sign-in file %s can be read by other users (mode %04o); chmod 600 it", f.path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return d, nil
	}
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return tokensFileData{}, fmt.Errorf("the MCP sign-in file %s: %w", f.path, err)
	}
	return d, nil
}

// Load is the grant under key, nil for none.
func (f *fileTokens) Load(_ context.Context, key mcpclient.TokenKey) (*mcpclient.TokenRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, err := f.read()
	if err != nil {
		return nil, err
	}
	return d.Grants[tokensKey(key)], nil
}

// Save keeps rec under key, the file replaced whole.
func (f *fileTokens) Save(_ context.Context, key mcpclient.TokenKey, rec *mcpclient.TokenRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := private.SecureDir(filepath.Dir(f.path)); err != nil {
		return err
	}
	d, err := f.read()
	if err != nil {
		return err
	}
	if d.Grants == nil {
		d.Grants = map[string]*mcpclient.TokenRecord{}
	}
	d.Grants[tokensKey(key)] = rec
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.path), ".mcp-tokens-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), f.path)
}
