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
// (internal/authn).

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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
	"time"

	workspace "github.com/ChristopherDavenport/agentworkspace"
	wsremote "github.com/ChristopherDavenport/agentworkspace/remote"
	wstunnel "github.com/ChristopherDavenport/agentworkspace/tunnel"

	"github.com/ChristopherDavenport/dax/internal/authn"
	"github.com/ChristopherDavenport/dax/internal/config"
	"github.com/ChristopherDavenport/dax/internal/render"
	"github.com/ChristopherDavenport/dax/tool"
)

// checkConnectURL refuses a hub URL -connect would send its token or
// the workspace over in the clear, or with credentials in it: wss://,
// or ws:// to a loopback host alone, the tunnel's own rule, checked
// before anything starts.
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
		return fmt.Errorf("workspace: -connect %q: ws:// only to a loopback host, since the token and the workspace would cross the network in the clear; use wss://", s)
	}
	return nil
}

// parseScopes is -scopes: read, write and exec, comma-separated, each
// once; none, or one of another name, is refused.
func parseScopes(list string) ([]wsremote.Scope, error) {
	var scopes []wsremote.Scope
	for v := range strings.SplitSeq(list, ",") {
		s := wsremote.Scope(strings.TrimSpace(v))
		switch s {
		case "":
			continue
		case wsremote.ScopeRead, wsremote.ScopeWrite, wsremote.ScopeExec:
		default:
			return nil, fmt.Errorf("workspace: -scopes: no scope %q; want read, write and exec, comma-separated", s)
		}
		if !slices.Contains(scopes, s) {
			scopes = append(scopes, s)
		}
	}
	if len(scopes) == 0 {
		return nil, errors.New("workspace: -scopes: none given; want read, write and exec, comma-separated")
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
// TLS 1.2 at least, verified against the system's roots and, when
// caFile names one, its certificate too; no Timeout, which would end
// the tunnel. The tunnel follows no redirect whatever it says.
func tunnelHTTPClient(caFile string) (*http.Client, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("workspace: -tls-ca: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
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

// dial is dax workspace -connect: the directory served as a
// workspace.Local through a tunnel to the hub at w.connect, dialled
// again whenever it drops, until a signal or the context's end. The
// token goes in the upgrade's Authorization header and nowhere else.
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
	dir := w.dir
	if dir == "" {
		if dir, err = os.Getwd(); err != nil {
			return err
		}
	}
	ws, err := workspace.NewLocal(dir, tool.DefaultEnv(w.pass))
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	closed := make(chan struct{})
	closeWS := func() { ws.Close(); close(closed) }
	scopes := make([]string, len(w.scopes))
	for i, s := range w.scopes {
		scopes[i] = string(s)
	}
	fmt.Fprintln(out, "dax workspace: serving "+render.Clean(ws.Root())+" to the hub at "+render.Clean(w.connect)+", which may "+strings.Join(scopes, ", "))
	fmt.Fprintln(out, "token: $"+config.TunnelTokenEnv)

	stop := ctx
	if w.signals {
		var cancel context.CancelFunc
		stop, cancel = signal.NotifyContext(ctx, stopSignals...)
		defer cancel()
	}
	header := func(context.Context) (http.Header, error) {
		return http.Header{"Authorization": {"Bearer " + w.token}}, nil
	}
	// The tunnel's own log, on standard error: each connection made and
	// lost, and why; it holds no credential.
	lg := slog.New(slog.NewTextHandler(out, nil))
	served := make(chan error, 1)
	go func() {
		served <- wstunnel.Serve(stop, w.connect, ws,
			wstunnel.WithScopes(w.scopes...), wstunnel.WithHTTPClient(hc), wstunnel.WithHeader(header), wstunnel.WithLogger(lg))
	}()
	if w.ready != nil {
		w.ready(w.connect)
	}
	select {
	case err := <-served:
		closeWS()
		if stop.Err() == nil {
			// It could not serve at all: the hub refused the token at
			// the first upgrade, or it refused again past the lease.
			return fmt.Errorf("workspace: -connect: %w", err)
		}
		return nil
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
	if h.Workspace != nil {
		closed := make(chan struct{})
		go func() { h.Workspace.Close(); close(closed) }()
		t := time.NewTimer(hubCloseWait)
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

// errNotEnrolled is an upgrade whose token enrolls no workspace; the
// hub sends none of its text.
var errNotEnrolled = errors.New("workspace.hub: no known token")

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
// refused before the upgrade is answered. out is told where it waits,
// and what to run on the workspace's host (prog, this program's name,
// with -connect), never the token.
func startHub(s config.HubSettings, token, prog string, out io.Writer) (*hostedHub, error) {
	cfg, err := loadTLS("workspace.hub", s.TLSCert, s.TLSKey)
	if err != nil {
		return nil, err
	}
	ln, scheme, err := listenHub(s.Listen, cfg)
	if err != nil {
		return nil, err
	}
	// The one token names the one workspace. The Authenticator's shape
	// is the tunnel's Enrollment: a request's name, or a refusal.
	enroll := authn.New(map[string]string{token: s.Name}).Authenticator(errNotEnrolled)
	hub, err := wstunnel.NewHub(wstunnel.WithEnrollment(enroll), wstunnel.WithMaxWorkspaces(1))
	if err != nil {
		ln.Close()
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle(config.TunnelPath, hub)
	// No ReadTimeout or WriteTimeout: the upgrade's connection is the
	// tunnel, every stream of the workspace's on it, for as long as the
	// session lasts.
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: serveIdle}
	go srv.Serve(ln)
	h := &hostedHub{URL: scheme + "://" + servedHost(ln.Addr()) + config.TunnelPath, s: s, hub: hub, srv: srv}
	h.connect = config.TunnelTokenEnv + "=... " + render.Clean(prog) + " workspace -connect " + render.Clean(h.URL)
	fmt.Fprintf(out, "workspace.hub: waiting up to %s for workspace %s at %s; on its host run: %s\n", s.Wait, s.Name, render.Clean(h.URL), h.connect)
	return h, nil
}

// await waits up to the hub's wait for its workspace to dial in, and
// takes it as h.Workspace, remote's client through the tunnel under the
// identity dax records, its ref config.TunnelRef(name). One that never
// connects is an error saying what to run on its host.
func (h *hostedHub) await(ctx context.Context) error {
	wctx, cancel := context.WithTimeout(ctx, h.s.Wait)
	defer cancel()
	ws, err := h.hub.Workspace(wctx, h.s.Name)
	if err != nil {
		if errors.Is(err, wstunnel.ErrNotConnected) && ctx.Err() == nil {
			return fmt.Errorf("workspace.hub: workspace %s did not connect to %s within %s; on its host run: %s", h.s.Name, render.Clean(h.URL), h.s.Wait, h.connect)
		}
		return fmt.Errorf("workspace.hub: %w", err)
	}
	h.Workspace = identify(ws)
	return nil
}

// listenHub is where a session's hub listens: wss on addr with cfg,
// which may then be any address, and ws on a loopback address
// otherwise. It returns the URL scheme.
func listenHub(addr string, cfg *tls.Config) (net.Listener, string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, "", fmt.Errorf("workspace.hub.listen %q: %w", addr, err)
	}
	if cfg != nil {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, "", fmt.Errorf("workspace.hub.listen: %w", err)
		}
		return tls.NewListener(ln, cfg), "wss", nil
	}
	if port == "" || port == "0" {
		port = "7780"
	}
	refuse := fmt.Errorf("workspace.hub.listen %q: without tls_cert and tls_key the hub listens on loopback only (127.0.0.1, ::1 or localhost); the workspace's host reaches it through a tunnel (ssh -R %s:127.0.0.1:%s <that host>), or give the hub a certificate", addr, port, port)
	if !isLoopbackHost(host) {
		return nil, "", refuse
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", fmt.Errorf("workspace.hub.listen: %w", err)
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
	return "remote " + name + " through the hub at " + h.URL + " · " + h.Workspace.Root() + startsNoProcess(h.Workspace)
}
