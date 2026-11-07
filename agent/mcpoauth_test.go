package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool/mcpclient"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// fakeAS is an authorization server on its own origin: metadata,
// dynamic registration, an authorize endpoint that consents at once by
// redirecting with a code, and a token endpoint; it records every
// Authorization header it is sent.
type fakeAS struct {
	srv                              *httptest.Server
	registered, exchanged, refreshed atomic.Int32
	issued                           atomic.Int32
	mu                               sync.Mutex
	authHeaders                      []string
	expiresIn                        int
}

func newFakeAS(t *testing.T) *fakeAS {
	t.Helper()
	as := &fakeAS{expiresIn: 3600}
	mux := http.NewServeMux()
	as.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		as.mu.Lock()
		as.authHeaders = append(as.authHeaders, r.Header.Get("Authorization"))
		as.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(as.srv.Close)
	issuer := as.srv.URL
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token",
			"registration_endpoint": issuer + "/register", "response_types_supported": []string{"code"},
			"grant_types_supported": []string{"authorization_code", "refresh_token"}, "code_challenge_methods_supported": []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"client_secret_post", "none"},
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		as.registered.Add(1)
		var md struct {
			RedirectURIs []string `json:"redirect_uris"`
		}
		_ = json.NewDecoder(r.Body).Decode(&md)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"client_id": "dyn-client", "client_secret": "dyn-secret", "redirect_uris": md.RedirectURIs})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		back, err := url.Parse(q.Get("redirect_uri"))
		if err != nil {
			http.Error(w, "bad redirect", http.StatusBadRequest)
			return
		}
		v := back.Query()
		v.Set("code", "code-1")
		v.Set("state", q.Get("state"))
		back.RawQuery = v.Encode()
		http.Redirect(w, r, back.String(), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			as.exchanged.Add(1)
		case "refresh_token":
			as.refreshed.Add(1)
		}
		n := as.issued.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("at-%d", n), "refresh_token": fmt.Sprintf("rt-%d", n), "token_type": "Bearer", "expires_in": as.expiresIn})
	})
	return as
}

// protectedServer is an MCP server whose /mcp needs a token the fake
// authorization server issued, naming it in its protected-resource
// metadata.
func protectedServer(t *testing.T, as *fakeAS) *httptest.Server {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "hosted", Version: "1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "where", Description: "report where the server runs"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "behind OAuth"}}}, struct{}{}, nil
		})
	mux := http.NewServeMux()
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	prm := &oauthex.ProtectedResourceMetadata{Resource: ts.URL + "/mcp", AuthorizationServers: []string{as.srv.URL}}
	mux.Handle("/.well-known/oauth-protected-resource/mcp", auth.ProtectedResourceMetadataHandler(prm))
	mux.Handle("/.well-known/oauth-protected-resource", auth.ProtectedResourceMetadataHandler(prm))
	verify := func(_ context.Context, tok string, _ *http.Request) (*auth.TokenInfo, error) {
		if !strings.HasPrefix(tok, "at-") {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{Expiration: time.Now().Add(time.Hour)}, nil
	}
	mux.Handle("/mcp", auth.RequireBearerToken(verify, &auth.RequireBearerTokenOptions{ResourceMetadataURL: ts.URL + "/.well-known/oauth-protected-resource/mcp"})(
		mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)))
	return ts
}

// browser stands in for the person: it reads the consent line dax
// prints, opens the URL and follows the redirect to dax's loopback.
type browser struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	opened atomic.Int32
}

func (b *browser) Write(p []byte) (int, error) {
	b.mu.Lock()
	b.buf.Write(p)
	text := b.buf.String()
	b.mu.Unlock()
	if u := regexp.MustCompile(`https?://\S+/authorize\?\S+`).FindString(text); u != "" && b.opened.CompareAndSwap(0, 1) {
		go func() {
			resp, err := http.Get(u)
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}
	return len(p), nil
}

func (b *browser) said() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A URL server with an oauth block signs in through the authorization
// server its metadata names: discovery, one registration, one consent
// (the URL printed, the redirect on a loopback port), the exchange. The
// grant is kept in a private file under ~/.dax, and a session started
// again refreshes from it and asks nobody. The access token goes to the
// server's origin alone (#175).
func TestAnMCPServerSignsInWithOAuth(t *testing.T) {
	as := newFakeAS(t)
	ts := protectedServer(t, as)
	b := &browser{}
	defer swapConsentOut(b)()
	ctx := context.Background()
	o := options(t, &scripted{})
	o.MCP = []MCPServer{{Name: "hosted", URL: ts.URL + "/mcp", OAuth: &OAuth{}}}
	s, err := New(ctx, o)
	if err != nil {
		t.Fatalf("%v; dax said %q", err, b.said())
	}
	if got := leak(t, s, "mcp__hosted__where"); got != "behind OAuth" {
		t.Errorf("where = %q", got)
	}
	s.Close()
	if as.registered.Load() != 1 || as.exchanged.Load() != 1 || b.opened.Load() != 1 {
		t.Errorf("registered %d, exchanged %d, consents %d: want one each", as.registered.Load(), as.exchanged.Load(), b.opened.Load())
	}
	if !strings.Contains(b.said(), "mcp hosted") {
		t.Errorf("the consent line %q does not name the server", b.said())
	}
	file := filepath.Join(o.UserDir, "mcp-tokens.json")
	fi, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("the token file's mode %04o lets others read it", fi.Mode().Perm())
	}
	data, _ := os.ReadFile(file)
	if !bytes.Contains(data, []byte("rt-1")) {
		t.Errorf("the token file holds no grant: %s", data)
	}

	// Started again: the stored grant, expired, is refreshed; nobody is
	// asked and nothing registers again.
	var kept map[string]map[string]map[string]any
	if err := json.Unmarshal(data, &kept); err != nil {
		t.Fatal(err)
	}
	for _, g := range kept["grants"] {
		g["Token"].(map[string]any)["expiry"] = time.Now().Add(-time.Hour).Format(time.RFC3339)
	}
	data, _ = json.Marshal(kept)
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	o2 := o
	o2.Root = filepath.Join(t.TempDir(), "sessions")
	s2, err := New(ctx, o2)
	if err != nil {
		t.Fatal(err)
	}
	if got := leak(t, s2, "mcp__hosted__where"); got != "behind OAuth" {
		t.Errorf("after a restart, where = %q", got)
	}
	s2.Close()
	if b.opened.Load() != 1 || as.registered.Load() != 1 || as.refreshed.Load() == 0 {
		t.Errorf("after a restart: consents %d, registered %d, refreshed %d; want no consent, no registration, a refresh", b.opened.Load(), as.registered.Load(), as.refreshed.Load())
	}
	as.mu.Lock()
	defer as.mu.Unlock()
	for _, h := range as.authHeaders {
		if strings.Contains(h, "at-") {
			t.Errorf("the authorization server was sent the MCP access token: %q", h)
		}
	}
}

// The authorization client goes to the server's origin for its
// metadata and to the authorization servers that metadata names, and
// nowhere else; a redirect off them is not followed (#175).
func TestTheOAuthGuardHoldsDiscoveryToTheNamedServer(t *testing.T) {
	var got []string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = append(got, r.URL.Host)
		body := `{}`
		if strings.HasPrefix(r.URL.Path, "/.well-known/oauth-protected-resource") {
			body = `{"resource":"https://mcp.example/mcp","authorization_servers":["https://as.example"]}`
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	g := newAuthGuard("https://mcp.example:443", rt)
	send := func(raw string) error {
		req, _ := http.NewRequest("GET", raw, nil)
		resp, err := g.RoundTrip(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, bufio.NewReader(resp.Body))
			resp.Body.Close()
		}
		return err
	}
	if err := send("https://as.example/.well-known/oauth-authorization-server"); err == nil {
		t.Error("the authorization server was reached before the metadata named it")
	}
	if err := send("https://mcp.example/.well-known/oauth-protected-resource/mcp"); err != nil {
		t.Fatal(err)
	}
	if err := send("https://as.example/.well-known/oauth-authorization-server"); err != nil {
		t.Errorf("the named authorization server: %v", err)
	}
	if err := send("https://evil.example/token"); err == nil {
		t.Error("another origin was reached")
	}
	req, _ := http.NewRequest("GET", "https://evil.example/cb", nil)
	if err := g.redirect(req, nil); err == nil {
		t.Error("a redirect off the named origins was followed")
	}
}

// A remote server's metadata cannot send the sign-in to this machine:
// an authorization server on a loopback host is named only by a server
// that is itself on loopback, so a hosted server cannot make dax POST
// a registration or a code to a local service (#175, review M1).
func TestARemoteServerCannotNameALoopbackAuthorizationServer(t *testing.T) {
	prmNaming := func(as ...string) http.RoundTripper {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body := `{}`
			if strings.HasPrefix(r.URL.Path, "/.well-known/oauth-protected-resource") {
				b, _ := json.Marshal(map[string]any{"resource": "x", "authorization_servers": as})
				body = string(b)
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		})
	}
	send := func(g *authGuard, raw string) error {
		req, _ := http.NewRequest("POST", raw, strings.NewReader(`{}`))
		resp, err := g.RoundTrip(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		return err
	}
	local := []string{"http://127.0.0.1:8080", "https://localhost:9443", "http://[::1]:7000", "https://127.0.0.2"}
	remote := newAuthGuard("https://mcp.example:443", prmNaming(append(local, "https://as.example")...))
	if err := send(remote, "https://mcp.example/.well-known/oauth-protected-resource/mcp"); err != nil {
		t.Fatal(err)
	}
	for _, as := range local {
		if err := send(remote, as+"/register"); err == nil {
			t.Errorf("a remote server's metadata sent the sign-in to %s on this machine", as)
		}
	}
	if err := send(remote, "https://as.example/register"); err != nil {
		t.Errorf("the remote server's own authorization server: %v", err)
	}

	// A server on loopback may name an authorization server on loopback.
	loop := newAuthGuard("http://127.0.0.1:3000", prmNaming("http://127.0.0.1:8080"))
	if err := send(loop, "http://127.0.0.1:3000/.well-known/oauth-protected-resource"); err != nil {
		t.Fatal(err)
	}
	if err := send(loop, "http://127.0.0.1:8080/register"); err != nil {
		t.Errorf("a loopback server's loopback authorization server: %v", err)
	}
}

// A server that refuses streamable HTTP is reached over HTTP+SSE, once,
// and the user is told (#175).
func TestAServerThatRefusesStreamableHTTPIsReachedOverSSE(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "legacy", Version: "1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "where", Description: "report where the server runs"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "over SSE"}}}, struct{}{}, nil
		})
	sse := mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts := httptest.NewServer(sse)
	t.Cleanup(ts.Close)
	var warned bytes.Buffer
	defer CaptureWarnings(&warned)()
	ctx := context.Background()
	o := options(t, &scripted{})
	o.MCP = []MCPServer{{Name: "legacy", URL: ts.URL}}
	s, err := New(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := leak(t, s, "mcp__legacy__where"); got != "over SSE" {
		t.Errorf("where = %q", got)
	}
	if !strings.Contains(warned.String(), "HTTP+SSE") {
		t.Errorf("warnings %q: want the fallback said", warned.String())
	}
}

// Two dax processes saving grants into one tokens file keep both: each
// save reads, changes and replaces the file under a lock on a file
// beside it, which an flock holds across processes. Two fileTokens on
// one path stand in for the two processes, since an flock is held by
// the open file, not the process (#175, review M2).
func TestTwoWritersOfTheTokensFileKeepEachOthersGrants(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dax", "mcp-tokens.json")
	a, b := &fileTokens{path: path}, &fileTokens{path: path}
	const n = 40
	var wg sync.WaitGroup
	for i, f := range []*fileTokens{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range n {
				key := mcpclient.TokenKey{Endpoint: fmt.Sprintf("https://s%d-%d.example/mcp", i, j), Subject: "me"}
				if err := f.Save(ctx, key, &mcpclient.TokenRecord{ClientID: key.Endpoint}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	for i := range 2 {
		for j := range n {
			key := mcpclient.TokenKey{Endpoint: fmt.Sprintf("https://s%d-%d.example/mcp", i, j), Subject: "me"}
			if rec, err := a.Load(ctx, key); err != nil || rec == nil {
				t.Fatalf("the grant of %s was lost (%v)", key.Endpoint, err)
			}
		}
	}
}

// The consent's loopback listener answers only the redirect of this
// sign-in: a request whose state is not the authorization URL's, such
// as a web page's hit on the port, is turned away and the wait goes on
// (review low 5).
func TestTheConsentWaitsForItsOwnState(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	cb := fmt.Sprintf("http://127.0.0.1:%d/callback", port)
	var out bytes.Buffer
	defer swapConsentOut(&out)()
	s := &Session{}
	fetch := s.consent("hosted", port, ln, nil)
	type got struct {
		res *auth.AuthorizationResult
		err error
	}
	done := make(chan got, 1)
	go func() {
		res, err := fetch(context.Background(), &auth.AuthorizationArgs{URL: "https://as.example/authorize?state=right-state&client_id=x"})
		done <- got{res, err}
	}()
	hit := func(q string) int {
		for range 50 {
			resp, err := http.Get(cb + "?" + q)
			if err == nil {
				resp.Body.Close()
				return resp.StatusCode
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("the callback never listened")
		return 0
	}
	if code := hit("error=access_denied&state=wrong"); code == http.StatusOK {
		t.Errorf("a stranger's error was answered %d", code)
	}
	if code := hit("code=stolen"); code == http.StatusOK {
		t.Errorf("a redirect with no state was answered %d", code)
	}
	select {
	case g := <-done:
		t.Fatalf("the sign-in ended on another's request: %+v %v", g.res, g.err)
	case <-time.After(100 * time.Millisecond):
	}
	if code := hit("code=good&state=right-state"); code != http.StatusOK {
		t.Errorf("the sign-in's own redirect: %d", code)
	}
	g := <-done
	if g.err != nil || g.res == nil || g.res.Code != "good" {
		t.Fatalf("got %+v %v, want the code of the matching redirect", g.res, g.err)
	}
}

// A preregistered client's secret is read from its variable each time
// and never kept in the tokens file; a dynamically registered client's,
// which has no variable, is kept (review low 6).
func TestTheClientSecretFromTheEnvironmentIsNotKept(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dax", "mcp-tokens.json")
	t.Setenv("DAX_TEST_CLIENT_SECRET", "first")
	store := secretFromEnv(&fileTokens{path: path}, "corp", "DAX_TEST_CLIENT_SECRET")
	key := mcpclient.TokenKey{Endpoint: "https://mcp.example/mcp", Subject: "me"}
	if err := store.Save(ctx, key, &mcpclient.TokenRecord{ClientID: "corp", ClientSecret: "first", TokenURL: "https://as.example/token"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if bytes.Contains(data, []byte("first")) {
		t.Errorf("the tokens file holds the client secret: %s", data)
	}
	t.Setenv("DAX_TEST_CLIENT_SECRET", "rotated")
	rec, err := store.Load(ctx, key)
	if err != nil || rec == nil || rec.ClientSecret != "rotated" {
		t.Fatalf("loaded %+v (%v), want the secret from the variable now", rec, err)
	}
	dyn := mcpclient.TokenKey{Endpoint: "https://other.example/mcp", Subject: "me"}
	if err := store.Save(ctx, dyn, &mcpclient.TokenRecord{ClientID: "dyn-client", ClientSecret: "dyn-secret"}); err != nil {
		t.Fatal(err)
	}
	if rec, _ := store.Load(ctx, dyn); rec == nil || rec.ClientSecret != "dyn-secret" {
		t.Errorf("a registered client's secret: %+v", rec)
	}
}

// The redirect's port is bound when the handler is made and held until
// the session has opened, so no other process takes it between the
// registration that names it and the consent (review low 7).
func TestTheCallbackPortIsHeldFromTheStart(t *testing.T) {
	ctx := context.Background()
	o := options(t, &scripted{})
	s := &Session{opts: o}
	h, err := s.oauthHandler(ctx, MCPServer{Name: "hosted", URL: "https://mcp.example/mcp", OAuth: &OAuth{}}, "https://mcp.example:443")
	if err != nil || h == nil {
		t.Fatal(err)
	}
	s.cbMu.Lock()
	n := len(s.callbacks)
	var addr string
	if n == 1 {
		addr = s.callbacks[0].Addr().String()
	}
	s.cbMu.Unlock()
	if n != 1 {
		t.Fatalf("%d listeners held, want the redirect's", n)
	}
	if ln, err := net.Listen("tcp", addr); err == nil {
		ln.Close()
		t.Fatalf("%s was free for another process", addr)
	}
	s.releaseCallbacks()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("%s still held after the session opened: %v", addr, err)
	}
	ln.Close()
}

// A 4xx to the initialize probe moves a server to HTTP+SSE only when a
// GET then opens an SSE stream whose first event is the endpoint, as
// the transports' backwards compatibility has a client check: a
// streamable server that answers 400 for a missing credential stays on
// streamable HTTP (review low 12).
func TestOnlyAnSSEServerIsMovedToSSE(t *testing.T) {
	streamable400 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.Error(w, "no", http.StatusMethodNotAllowed)
			return
		}
		http.Error(w, "missing x-api-key", http.StatusBadRequest)
	}))
	defer streamable400.Close()
	if code := refusesStreamable(context.Background(), streamable400.Client(), streamable400.URL); code != 0 {
		t.Errorf("a streamable server's 400 moved it to SSE (%d)", code)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "legacy", Version: "1"}, nil)
	legacy := httptest.NewServer(mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	defer legacy.Close()
	if code := refusesStreamable(context.Background(), legacy.Client(), legacy.URL); code == 0 {
		t.Error("an HTTP+SSE server was not moved to SSE")
	}
}
