package dax

// This file is the served workspace turned around, both ends of
// agentworkspace's tunnel: `dax workspace -connect`, a directory on a
// host the session cannot reach, which dials out to the session's hub
// and serves the workspace through that connection; and
// workspace.hub, a session that hosts the hub, waits for its workspace
// to dial in, and acts in it as -workspace acts in one it dials. The
// tunnel carries agentworkspace's remote unchanged, so what runs where
// is as in workspace.go; only who dials whom differs. The library ships
// no authentication: the one token, enrolling one name, is dax's
// (internal/authn), and it is never sent. Before each upgrade -connect
// asks the hub for a nonce, which the hub issues statelessly; the
// upgrade carries an HMAC under the token of the hub's name, that nonce
// and the run's instance id, which the hub takes only for a nonce it
// issued, within 30 seconds, once; the hub's 101 carries its own HMAC
// of the same, which -connect checks before it serves anything
// (authn.Prover and authn.Hub). Over ws:// to loopback behind an ssh -R
// forward, a local user who takes the port first gets no token, no
// shell, and no upgrade the hub would take later. The proof does not
// bind the connection: an impostor that can reach the hub while it
// holds the port relays to it, so where the hub is reachable otherwise,
// wss:// is needed; authn's proof.go says what else it leaves.

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"time"

	wsremote "github.com/ChristopherDavenport/agentworkspace/remote"
	wstunnel "github.com/ChristopherDavenport/agentworkspace/tunnel"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/internal/authn"
	"github.com/ChristopherDavenport/dax/internal/config"
	"github.com/ChristopherDavenport/dax/internal/render"
)

// checkConnectURL refuses a hub URL -connect would serve the workspace
// over in the clear across a network, or with credentials in it:
// wss://, or ws:// to a loopback host alone, the tunnel's own rule,
// checked before anything starts. The token crosses neither (the proof
// is a MAC), but the workspace's files and commands would cross ws://
// unencrypted and, since the proof does not bind the connection,
// unbound to the hub.
func checkConnectURL(s string) error {
	if s == "" {
		return errors.New("workspace: -connect: give the hub's wss:// URL")
	}
	u, err := neturl.Parse(s)
	switch {
	case err != nil || u.Host == "" || (u.Scheme != "wss" && u.Scheme != "ws"):
		return fmt.Errorf("workspace: -connect %q: want the wss:// URL of a session's hub, such as wss://host:7780%s", s, config.TunnelPath)
	case u.User != nil:
		return fmt.Errorf("workspace: -connect %q carries credentials; the token goes in $%s", u.Redacted(), config.TunnelTokenEnv)
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return fmt.Errorf("workspace: -connect %q: a query or fragment; give the hub's address alone", s)
	case u.Scheme == "ws" && !isLoopbackHost(strings.ToLower(u.Hostname())):
		return fmt.Errorf("workspace: -connect %q: ws:// only to a loopback host, since the workspace's files and commands would cross the network in the clear, bound to no hub; use wss://", s)
	}
	return nil
}

// parseScopes is -scopes: read, write, exec and servers,
// comma-separated, each once; none, or one of another name, is refused.
func parseScopes(list string) ([]wsremote.Scope, error) {
	var scopes []wsremote.Scope
	for v := range strings.SplitSeq(list, ",") {
		s := wsremote.Scope(strings.TrimSpace(v))
		switch s {
		case "":
			continue
		case wsremote.ScopeRead, wsremote.ScopeWrite, wsremote.ScopeExec, wsremote.ScopeServers:
		default:
			return nil, fmt.Errorf("workspace: -scopes: no scope %q; want read, write, exec and servers, comma-separated", s)
		}
		if !slices.Contains(scopes, s) {
			scopes = append(scopes, s)
		}
	}
	if len(scopes) == 0 {
		return nil, errors.New("workspace: -scopes: none given; want read, write, exec and servers, comma-separated")
	}
	return scopes, nil
}

// takeTunnelToken is the token that enrolls the workspace at a hub,
// read from env and taken out of this process's environment at once,
// before anything here starts a process. It must be at least
// minTokenBytes long, as a served token must: it is the only thing
// between the network and the workspace.
func takeTunnelToken(mode, env string) (string, error) {
	token := os.Getenv(env)
	os.Unsetenv(env)
	switch {
	case token == "":
		return "", fmt.Errorf("%s: no token: set $%s to the token the workspace and its hub share (openssl rand -base64 32 makes one)", mode, env)
	case len(token) < minTokenBytes:
		return "", fmt.Errorf("%s: $%s is %d bytes; the token must be at least %d (openssl rand -base64 32 makes one)", mode, env, len(token), minTokenBytes)
	}
	return token, nil
}

// tunnelHTTPClient is the client -connect makes its upgrade through:
// TLS 1.2 at least, verified against the system's roots or, when caFile
// names one, against its certificate in their place, as
// workspace.tls_ca and attach's -tls-ca are; no Timeout, which would
// end the tunnel. The tunnel follows no redirect whatever it says.
func tunnelHTTPClient(caFile string) (*http.Client, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("workspace: -tls-ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("workspace: -tls-ca %s holds no PEM certificate", caFile)
		}
		cfg.RootCAs = pool
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = cfg
	return &http.Client{
		Transport:     t,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// instanceHeader carries a dax workspace -connect run's instance id on
// each upgrade: random, made once for the run, so its own redials keep
// it and another run, here or on another host, has another. A session's
// hub takes the first id that connects and refuses every other for its
// life, so its workspace is one host's directory and stays it.
const instanceHeader = "Dax-Workspace-Instance"

// newInstance is a run's instance id: 16 random bytes, in hex.
func newInstance() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// validInstance reports whether id is one newInstance makes.
func validInstance(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && strings.ToLower(id) == id
}

// dialWatch is -connect's transport for the upgrades: it passes each
// on, and until the first is answered it ends the run, by stop, at an
// answer no redial would change, which the tunnel would otherwise try
// again for ever: a hub's certificate the roots do not verify, or no
// hub at the path (404). Not up yet, refused or timed out are tried
// again, as the tunnel does; once connected, everything is.
type dialWatch struct {
	base http.RoundTripper
	stop context.CancelFunc

	mu        sync.Mutex
	connected bool
	fatal     error
}

func (d *dialWatch) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := d.base.RoundTrip(r)
	d.mu.Lock()
	defer d.mu.Unlock()
	var cert *tls.CertificateVerificationError
	switch {
	case err == nil && resp.StatusCode == http.StatusSwitchingProtocols:
		d.connected = true
	case d.connected || d.fatal != nil:
	case err != nil && errors.As(err, &cert):
		d.fatal = fmt.Errorf("the hub's certificate is not one the roots verify (give its CA with -tls-ca): %w", cert)
		d.stop()
	case err == nil && resp.StatusCode == http.StatusNotFound:
		d.fatal = fmt.Errorf("the hub answered %s: no hub at that path (a session's hub takes upgrades at %s)", resp.Status, config.TunnelPath)
		d.stop()
	}
	return resp, err
}

// halt ends the run, in any phase, with err, unless something already
// has.
func (d *dialWatch) halt(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.fatal == nil {
		d.fatal = err
		d.stop()
	}
}

// failure is what ended the run, or nil.
func (d *dialWatch) failure() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.fatal
}

// dial is dax workspace -connect: the directory served as a
// workspace.Local through a tunnel to the hub at w.connect, dialled
// again whenever it drops, until a signal or the context's end. The
// token is never sent: before each upgrade it asks the hub for a nonce,
// and the upgrade carries the MAC of the hub's name (-name), that nonce
// and the run's instance id under the token (authn.Prover); nothing is
// served to a hub whose 101 does not carry its own MAC of them. A hub
// that fails the proof, or that issues no nonce, ends the run in any
// phase, first dial or redial: it is not the session's hub, and is not
// dialled again, so an impostor that takes the port after a drop gets
// one upgrade at most.
// At a stop every process the session started here ends before it
// returns, each wait bounded as -listen's are.
func (w *workspaceServer) dial(ctx context.Context) error {
	out := w.out
	if out == nil {
		out = os.Stderr
	}
	hc, err := tunnelHTTPClient(w.tlsCA)
	if err != nil {
		return err
	}
	if strings.HasPrefix(w.connect, "ws://") {
		// ws:// is to loopback alone (checkConnectURL), where the proof
		// holds against a user who takes the port and cannot reach the
		// hub: the upgrade carries nothing the hub would take later,
		// and nothing is served to a hub that cannot answer it. What it
		// does not do is bind the connection.
		u, _ := neturl.Parse(w.connect)
		fmt.Fprintln(out, "dax workspace: note: ws:// is not encrypted; the hub must prove it holds the token before anything is served, which holds behind ssh -o ExitOnForwardFailure=yes -R against a user of this host who takes "+render.Clean(u.Host)+", but not against one who can also reach the hub some other way and relay to it: then use wss:// with -tls-ca")
	}
	ws, err := w.open()
	if err != nil {
		return err
	}
	closed := make(chan struct{})
	closeWS := func() { ws.Close(); close(closed) }
	fmt.Fprintln(out, "dax workspace: serving "+render.Clean(ws.Root())+" to the hub at "+render.Clean(w.connect)+", which may "+scopeList(w.scopes))
	w.sayOffered(out)
	fmt.Fprintln(out, "token: $"+config.TunnelTokenEnv)

	stop := ctx
	if w.signals {
		var cancel context.CancelFunc
		stop, cancel = signal.NotifyContext(ctx, stopSignals...)
		defer cancel()
	}
	instance := newInstance()
	prover := authn.NewProver(w.token, w.hubName)
	serve, cancelServe := context.WithCancel(stop)
	defer cancelServe()
	watch := &dialWatch{base: hc.Transport, stop: cancelServe}
	hc.Transport = watch
	notTheHub := func(err error) error {
		return fmt.Errorf("whatever answered %s is not the session's hub: it could not prove it holds $%s, so nothing was served to it, and this run stops rather than dial it again. Another user of this host may hold the port: rotate $%s (start the session's hub with a new token) before you bring the ssh -R forward back, since one who fetched a nonce from the hub while the forward was up can replay this upgrade within %s (%w: %w)", render.Clean(w.connect), config.TunnelTokenEnv, config.TunnelTokenEnv, authn.NonceLifetime, wstunnel.ErrHubProof, err)
	}
	header := func(ctx context.Context) (http.Header, error) {
		fctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		nonce, err := authn.FetchNonce(fctx, hc, w.connect)
		switch {
		case errors.Is(err, authn.ErrNoNonce):
			// An answer, and not a dax hub's of this version: no redial
			// changes it.
			watch.halt(fmt.Errorf("the hub at %s issued no nonce: it is not a session's hub, or it is a dax older than %s, whose hub takes the token itself; upgrade both (%w)", render.Clean(w.connect), proofSince, err))
			return nil, err
		case err != nil:
			return nil, err
		}
		h := prover.Header(nonce, instance)
		h.Set(instanceHeader, instance)
		return h, nil
	}
	proof := func(req *http.Request, resp *http.Response) error {
		err := prover.CheckHub(req, resp, instance)
		if err != nil {
			// Whatever the phase: an impostor after a drop gets this
			// one upgrade and no other.
			watch.halt(notTheHub(err))
		}
		return err
	}
	// The tunnel's own log, on standard error: each connection made and
	// lost, and why; it holds no credential.
	lg := slog.New(slog.NewTextHandler(out, nil))
	served := make(chan error, 1)
	go func() {
		served <- wstunnel.Serve(serve, w.connect, ws,
			wstunnel.WithScopes(w.scopes...), wstunnel.WithHTTPClient(hc), wstunnel.WithHeader(header),
			wstunnel.WithHubProof(proof), wstunnel.WithLogger(lg))
	}()
	if w.ready != nil {
		w.ready(w.connect)
	}
	select {
	case err := <-served:
		closeWS()
		switch {
		case watch.failure() != nil:
			return fmt.Errorf("workspace: -connect: %w", watch.failure())
		case stop.Err() != nil:
			return nil
		case errors.Is(err, wstunnel.ErrHubProof):
			// The proof hook halts the run first; this is the same.
			return fmt.Errorf("workspace: -connect: %w", notTheHub(err))
		case errors.Is(err, wsremote.ErrUnauthenticated):
			return fmt.Errorf("workspace: -connect: the hub refused this workspace's proof: $%s is not the token the hub was given, -name is not the workspace name it enrolls, or the hub is a dax older than %s, whose hub takes the token itself (upgrade both) (%w)", config.TunnelTokenEnv, proofSince, err)
		case errors.Is(err, wsremote.ErrForbidden):
			// dax's hub answers 403 to a host other than the first
			// that connected, for the session's life.
			return fmt.Errorf("workspace: -connect: the hub's workspace is held by another host, the first that connected to it: stop that dax workspace -connect and start a new session for this one, or give this one a hub of its own (%w)", err)
		}
		// It could not serve at all: the hub refused the token at the
		// first upgrade, or it refused again past the lease.
		return fmt.Errorf("workspace: -connect: %w", err)
	case <-stop.Done():
	}
	// Serve's end closes the connection and the handler, which ends
	// every process the session started; the workspace's close ends
	// any Start left after that. A second signal cuts the waits short.
	again := make(chan os.Signal, 1)
	if w.signals {
		signal.Notify(again, stopSignals...)
		defer signal.Stop(again)
	}
	drain := w.drain
	if drain == 0 {
		drain = 3 * time.Second
	}
	hurried := false
	wait := func(done <-chan struct{}) bool {
		if hurried {
			return false
		}
		t := time.NewTimer(drain)
		defer t.Stop()
		select {
		case <-done:
			return true
		case <-t.C:
		case <-again:
			hurried = true
		}
		return false
	}
	ended := make(chan struct{})
	go func() { <-served; close(ended) }()
	wait(ended)
	go closeWS()
	if !wait(closed) && !hurried {
		fmt.Fprintln(out, "dax workspace: stopped with a process still ending")
	}
	return nil
}

// hostedHub is a session's workspace.hub: the hub, the server it is
// mounted on, and the workspace that dialled in.
type hostedHub struct {
	// Workspace is the one that dialled in, under the identity dax
	// records (identify).
	Workspace wsremote.Workspace
	// URL is the hub's upgrade URL, as the workspace's host dials it.
	URL     string
	s       config.HubSettings
	connect string // what to run on the workspace's host, without the token
	hub     *wstunnel.Hub
	srv     *http.Server
	once    sync.Once
	// closeBy, once closeSession has set it, is when the whole close
	// is due; zero gives the workspace's close hubCloseWait alone.
	closeBy time.Time
}

// hubCloseWait bounds the wait for the workspace's close before the
// hub is closed under it.
const hubCloseWait = 3 * time.Second

// Close closes the workspace, then the hub and every connection to it,
// then the server, each wait bounded. The workspace's close ends its
// session there, and the processes in it, over the tunnel; with its
// host gone it would wait for the host to come back for as long as
// remote's timeout, so past hubCloseWait the hub is closed, which fails
// it at once, and the host's lease ends those processes.
func (h *hostedHub) Close() error {
	var err error
	h.once.Do(func() { err = h.close() })
	return err
}

func (h *hostedHub) close() error {
	if h.Workspace != nil {
		closed := make(chan struct{})
		go func() { h.Workspace.Close(); close(closed) }()
		wait := hubCloseWait
		if !h.closeBy.IsZero() {
			// What the session's close left of the one deadline.
			wait = max(time.Until(h.closeBy), 0)
		}
		t := time.NewTimer(wait)
		select {
		case <-closed:
		case <-t.C:
		}
		t.Stop()
		h.hub.Close()
		<-closed
	}
	h.hub.Close()
	sctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	h.srv.Shutdown(sctx)
	return h.srv.Close()
}

// closeSession closes sess, and with hubs hosted (the workspace's, the
// servers workspace's; nil for none) or workspaces dialed, it and the
// hubs' Close after it within hubCloseWait in all: past it each hub is
// closed under the session, and each dialed workspace's requests given
// up (abandoner), which fails at once what still waits for a workspace
// that went away (the MCP servers' closes in it among them), and the
// close finishes on that, the record's included.
func closeSession(sess io.Closer, hubs []*hostedHub, dialed ...abandoner) error {
	hubs = slices.DeleteFunc(slices.Clone(hubs), func(h *hostedHub) bool { return h == nil })
	dialed = slices.DeleteFunc(slices.Clone(dialed), func(a abandoner) bool { return a == nil })
	if len(hubs) == 0 && len(dialed) == 0 {
		return sess.Close()
	}
	// One deadline for the session's close and the hubs' after it.
	by := time.Now().Add(hubCloseWait)
	for _, h := range hubs {
		h.closeBy = by
	}
	done := make(chan error, 1)
	go func() { done <- sess.Close() }()
	t := time.NewTimer(hubCloseWait)
	defer t.Stop()
	select {
	case err := <-done:
		return err
	case <-t.C:
	}
	for _, h := range hubs {
		h.hub.Close()
	}
	for _, a := range dialed {
		a.Abandon()
	}
	return <-done
}

// proofSince is the dax release whose hub and -connect first prove the
// token to each other with a nonce the hub issues; the two sides of an
// older one do not enroll with these.
const proofSince = "v0.0.31"

// errNotEnrolled is an upgrade whose proof enrolls no workspace; the
// hub sends none of its text.
var errNotEnrolled = errors.New("workspace.hub: no known token")

// errHeld is an upgrade proved with the token from another instance
// than the one that holds the workspace.
var errHeld = errors.New("workspace.hub: the workspace is held by another host")

// hubRequestTimeout bounds each of the workspace's requests that should
// be answered at once (remote.WithTimeout: a stat, a listing, a file's
// answer but not its content, a process's start, signal and close), in
// place of remote's 30 seconds. Through a tunnel a request made while
// the host is away waits for it to come back, so this is how long a
// tool waits on a host that dropped before it fails. It bounds no
// stream, wait or Exec, which only their contexts bound, nor a write,
// which its context alone does, and a session's renewal is already
// sent every 10 seconds within its own bound; a process's close is
// given this and 10 seconds more, past the server's grace.
const hubRequestTimeout = 10 * time.Second

// pin holds a hub's workspace to the first instance that enrolls: the
// enrollment takes an upgrade proved with the one token (its MAC of the
// name, a nonce the hub issued and the instance id, authn.Hub) as the
// one name, and
// the instance id of the first such upgrade; one proved with the token
// and another id is refused 403 before the hub sees it, for the hub's
// life, so a second host (or a second run on the first) cannot take the
// workspace over while the session describes the first.
type pin struct {
	name  string
	proof *authn.Hub
	hub   http.Handler
	log   *slog.Logger

	mu       sync.Mutex
	instance string
}

// enroll is the hub's Enrollment: the one name, for an upgrade proved
// with the token over a nonce the hub issued, the nonce taken so it
// serves no other upgrade, and the first id taken as the one that holds
// it.
func (p *pin) enroll(r *http.Request) (string, error) {
	id := r.Header.Get(instanceHeader)
	err := p.proof.Take(r, id)
	switch {
	case errors.Is(err, authn.ErrBusy):
		return "", fmt.Errorf("%w: %w", wstunnel.ErrTryLater, err)
	case errors.Is(err, authn.ErrUnknownNonce), errors.Is(err, authn.ErrStaleNonce), errors.Is(err, authn.ErrReplayedNonce):
		// Made with the token, so worth saying: a replay of an upgrade
		// someone captured, or one sent again or too slowly.
		p.log.Warn("refused an upgrade for workspace "+p.name+": "+err.Error(), "from", r.RemoteAddr)
		return "", errNotEnrolled
	case err != nil:
		return "", errNotEnrolled
	}
	name := p.name
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case !validInstance(id):
		return "", errNotEnrolled
	case p.instance == "":
		p.instance = id
	case p.instance != id:
		return "", errHeld
	}
	return name, nil
}

// hubProof is the hub's answer to an upgrade enroll took: its MAC of
// the name, the upgrade's nonce and its instance id under the token.
func (p *pin) hubProof(r *http.Request, _ string) (http.Header, error) {
	return p.proof.Header(r, r.Header.Get(instanceHeader))
}

// ServeHTTP answers a plain GET (no upgrade) with a nonce for the next
// upgrade (authn.FetchNonce asks for it), which costs the hub nothing to
// keep; refuses an upgrade proved with the token from another instance
// than the one that holds the workspace; and hands every other request
// to the hub.
func (p *pin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		if r.Header.Get("Origin") != "" {
			// No browser is a client, as the hub says of an upgrade.
			http.Error(w, "workspace.hub: a request from a web page", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set(authn.NonceHeader, p.proof.Nonce())
		w.WriteHeader(http.StatusOK)
		return
	}
	id := r.Header.Get(instanceHeader)
	// A look, not the enrollment: the nonce is taken there.
	if err := p.proof.Check(r, id); err == nil {
		p.mu.Lock()
		held := p.instance
		p.mu.Unlock()
		var why string
		switch {
		case !validInstance(id):
			why = "an upgrade with no instance id; dax workspace -connect sends one"
		case held != "" && id != held:
			why = "the workspace is held by another host, the first that connected; a second is refused for the session's life"
		}
		if why != "" {
			p.log.Warn("refused a host for workspace "+p.name+": "+why, "from", r.RemoteAddr)
			http.Error(w, "workspace.hub: "+why, http.StatusForbidden)
			return
		}
	}
	p.hub.ServeHTTP(w, r)
}

// hubLogger is where a session's hub says what its workspace's
// connection does (connected, dropped, refused): dax's warnings, which a
// front that has the screen holds and shows, never raw standard error
// under it.
func hubLogger(setting string) *slog.Logger {
	return slog.New(slog.NewTextHandler(render.CleanWriter(agent.Warnings()), &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})).With("dax", setting)
}

// hostHub hosts the hub s says and waits up to s.Wait for the
// workspace it names to dial in (startHub, then await).
func hostHub(ctx context.Context, s config.HubSettings, token, prog string, out io.Writer) (*hostedHub, error) {
	h, err := startHub(s, token, prog, out)
	if err != nil {
		return nil, err
	}
	if err := h.await(ctx); err != nil {
		h.Close()
		return nil, err
	}
	return h, nil
}

// startHub listens and serves the hub s says, at config.TunnelPath, on
// a loopback address unless s has a certificate. token is the one that
// enrolls the workspace as s.Name (takeTunnelToken); any other is
// refused before the upgrade is answered, and the token is never sent
// either way: the upgrade and the 101 each carry a MAC under it. out is told where it waits,
// and what to run on the workspace's host (prog, this program's name,
// with -connect), never the token.
func startHub(s config.HubSettings, token, prog string, out io.Writer) (*hostedHub, error) {
	cfg, err := loadTLSFiles(s.Setting(), "tls_cert", "tls_key", s.TLSCert, s.TLSKey)
	if err != nil {
		return nil, err
	}
	ln, scheme, err := listenHub(s.Setting(), s.Listen, cfg)
	if err != nil {
		return nil, err
	}
	// The one token enrolls the one workspace, held by the first
	// instance it enrolls: an upgrade proves it holds the token over a
	// nonce the hub issued, and the hub's 101 proves the hub does
	// (authn.Hub); a refusal's 401 names the proof's scheme.
	lg := hubLogger(s.Setting())
	p := &pin{name: s.Name, proof: authn.NewHub(token, s.Name), log: lg}
	hub, err := wstunnel.NewHub(wstunnel.WithEnrollment(p.enroll), wstunnel.WithProofHeader(p.hubProof),
		wstunnel.WithChallenge(authn.ProofChallenge), wstunnel.WithMaxWorkspaces(1), wstunnel.WithLogger(lg))
	if err != nil {
		ln.Close()
		return nil, err
	}
	p.hub = hub
	mux := http.NewServeMux()
	mux.Handle(config.TunnelPath, p)
	// No ReadTimeout or WriteTimeout: the upgrade's connection is the
	// tunnel, every stream of the workspace's on it, for as long as the
	// session lasts.
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: serveIdle}
	go srv.Serve(ln)
	h := &hostedHub{URL: scheme + "://" + servedHost(ln.Addr()) + config.TunnelPath, s: s, hub: hub, srv: srv}
	h.connect = config.TunnelTokenEnv + "=... " + render.Clean(prog) + " workspace -connect " + render.Clean(h.URL) + " -name " + s.Name
	if s.Field == config.ServersField+".hub" {
		// A servers workspace is granted its servers alone, which the
		// session requires.
		h.connect += " -scopes servers -servers FILE"
	}
	fmt.Fprintf(out, "%s: waiting up to %s for workspace %s at %s; on its host run: %s\n", s.Setting(), s.Wait, s.Name, render.Clean(h.URL), h.connect)
	return h, nil
}

// await waits up to the hub's wait for its workspace to dial in, and
// takes it as h.Workspace, remote's client through the tunnel under the
// identity dax records, its ref config.TunnelRef(name). One that never
// connects is an error saying what to run on its host.
func (h *hostedHub) await(ctx context.Context) error {
	wctx, cancel := context.WithTimeout(ctx, h.s.Wait)
	defer cancel()
	ws, err := h.hub.Workspace(wctx, h.s.Name, wsremote.WithTimeout(hubRequestTimeout))
	if err != nil {
		if errors.Is(err, wstunnel.ErrNotConnected) && ctx.Err() == nil {
			return fmt.Errorf("%s: workspace %s did not connect to %s within %s; on its host run: %s", h.s.Setting(), h.s.Name, render.Clean(h.URL), h.s.Wait, h.connect)
		}
		return fmt.Errorf("%s: %w", h.s.Setting(), err)
	}
	h.Workspace = identify(ws)
	return nil
}

// listenHub is where a session's hub listens: wss on addr with cfg,
// which may then be any address, and ws on a loopback address
// otherwise. It returns the URL scheme.
func listenHub(setting, addr string, cfg *tls.Config) (net.Listener, string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, "", fmt.Errorf("%s.listen %q: %w", setting, addr, err)
	}
	if cfg != nil {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, "", fmt.Errorf("%s.listen: %w", setting, err)
		}
		return tls.NewListener(ln, cfg), "wss", nil
	}
	if port == "" || port == "0" {
		port = "7780"
	}
	refuse := fmt.Errorf("%s.listen %q: without tls_cert and tls_key the hub listens on loopback only (127.0.0.1, ::1 or localhost); the workspace's host reaches it through a tunnel (ssh -o ExitOnForwardFailure=yes -R %s:127.0.0.1:%s <that host>), or give the hub a certificate", setting, addr, port, port)
	if !isLoopbackHost(strings.ToLower(host)) {
		return nil, "", refuse
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", fmt.Errorf("%s.listen: %w", setting, err)
	}
	if a, ok := ln.Addr().(*net.TCPAddr); !ok || !a.IP.IsLoopback() {
		ln.Close()
		return nil, "", refuse
	}
	return ln, "ws", nil
}

// hubLine is the banner's line about a workspace reached through the
// session's hub: remote, its name, the hub, its root, and whether it
// can start a process.
func hubLine(h *hostedHub, name string) string {
	return "remote " + name + " through the hub at " + render.Clean(h.URL) + " · " + render.Clean(h.Workspace.Root()) + startsNoProcess(h.Workspace)
}
