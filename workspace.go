package dax

// This file is the served workspace, both ends: `dax workspace`, a
// directory on this machine served whole over HTTP (agentworkspace's
// remote), and -workspace, a session whose tools act in one. It is the
// primitive cut: every stat, read, write and process crosses the wire
// on its own, and the tools, their facts and the policy stay with the
// session. `dax execute` is the other cut, at the tool calls, where the
// tools run where they act.

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"strconv"
	"time"

	workspace "github.com/ChristopherDavenport/agentworkspace"
	wsremote "github.com/ChristopherDavenport/agentworkspace/remote"

	"github.com/ChristopherDavenport/dax/internal/config"
	"github.com/ChristopherDavenport/dax/internal/render"
	"github.com/ChristopherDavenport/dax/tool"
)

// workspaceListen is where dax workspace serves by default: loopback,
// beside execute's 7777 and serve's 7878.
const workspaceListen = "127.0.0.1:7779"

// workspacePrincipal is the one principal dax workspace serves: the
// holder of its token, who may read, write and run anything.
const workspacePrincipal = "dax"

// runWorkspace is dax workspace, a mode with flags of its own that
// reads no config: it serves a directory as a workspace.Local over
// HTTP behind one bearer token, until a signal comes. The token is
// $DAX_WORKSPACE_TOKEN, or one made here and written to a file only the
// user can read, whose path alone is printed. It listens on loopback
// unless it is given a certificate; authentication is always on.
func runWorkspace(ctx context.Context, args []string, p program) error {
	fs := flag.NewFlagSet(p.name+" workspace", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: %s workspace [flags]\n\nserves a directory over HTTP as a workspace, for a session started elsewhere with -workspace to run its tools in; reads no config file.\n", p.name)
		fs.PrintDefaults()
	}
	dir := fs.String("dir", "", "directory to serve; default the current directory")
	listen := fs.String("listen", workspaceListen, "address to serve at; a loopback address unless -tls-cert and -tls-key are given; port 0 picks one")
	passEnv := fs.String("pass-env", "", "comma-separated variables that look like credentials to pass to the workspace's processes anyway")
	tlsCert := fs.String("tls-cert", "", "serve HTTPS with this certificate (PEM), with -tls-key; then -listen may be any address")
	tlsKey := fs.String("tls-key", "", "the private key (PEM) of -tls-cert")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("workspace: unexpected argument %q", fs.Arg(0))
	}
	pass, err := splitPassEnv("workspace", *passEnv)
	if err != nil {
		return err
	}
	return (&workspaceServer{name: p.name, dir: *dir, addr: *listen, tlsCert: *tlsCert, tlsKey: *tlsKey, pass: pass, signals: true}).run(ctx)
}

// workspaceServer is dax workspace's server.
type workspaceServer struct {
	name string
	// dir is the directory served; empty is the current one.
	dir string
	// addr is where to listen: a loopback address, or any with TLS.
	addr string
	// tlsCert and tlsKey are the certificate and key files it serves
	// HTTPS with; with neither it serves HTTP on loopback only.
	tlsCert, tlsKey string
	// pass are the credential-looking variables the workspace's
	// processes keep (-pass-env).
	pass []string
	// out is where it says where it serves; nil is standard error.
	out io.Writer
	// tokenDir is where a token made here is written; empty is
	// ~/.dax/serve.
	tokenDir string
	// signals stops serving on SIGINT and SIGTERM, besides the
	// context's end.
	signals bool
	// ready is told the base URL once the listener is up; tests set it.
	ready func(url string)
	// drain bounds each wait for the handler's close at a stop; zero is
	// three seconds.
	drain time.Duration
}

func (w *workspaceServer) run(ctx context.Context) error {
	out := w.out
	if out == nil {
		out = os.Stderr
	}
	cfg, err := loadTLS("dax workspace", w.tlsCert, w.tlsKey)
	if err != nil {
		return err
	}
	ln, scheme, err := listenWorkspace(w.addr, cfg)
	if err != nil {
		return err
	}
	defer ln.Close()
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
	defer ws.Close()
	// A token made here is named for the port and a random part, so two
	// served at once keep a file each.
	var nonce [4]byte
	rand.Read(nonce[:])
	port := "0"
	if a, ok := ln.Addr().(*net.TCPAddr); ok {
		port = strconv.Itoa(a.Port)
	}
	token, tokenFile, cleanup, err := makeToken(config.WorkspaceTokenEnv, w.tokenDir, "workspace-"+port+"-"+hex.EncodeToString(nonce[:])+".token")
	if err != nil {
		return err
	}
	defer cleanup()
	h, err := wsremote.NewHandler(ws, wsremote.WithAuthenticator(wsremote.BearerTokens(map[string]wsremote.Principal{
		token: {Name: workspacePrincipal, Scopes: []wsremote.Scope{wsremote.ScopeRead, wsremote.ScopeWrite, wsremote.ScopeExec}},
	})))
	if err != nil {
		return err
	}
	// No ReadTimeout or WriteTimeout: they would cut Exec's frames and a
	// process's pipes, which stream for as long as the process runs. An
	// idle connection is closed, so a client that only holds one open
	// holds no descriptor for long.
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: serveIdle}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	url := scheme + "://" + servedHost(ln.Addr())
	fmt.Fprintln(out, "dax workspace: serving "+render.Clean(ws.Root())+" at "+render.Clean(url))
	ca := ""
	if scheme == "https" {
		ca = `, "tls_ca": "<the certificate's CA, unless the system trusts it>"`
	}
	if tokenFile != "" {
		fmt.Fprintln(out, "token: "+render.Clean(tokenFile))
		fmt.Fprintln(out, "connect: "+config.WorkspaceTokenEnv+"=\"$(cat "+render.Clean(tokenFile)+")\" "+render.Clean(w.name)+" -workspace "+render.Clean(url))
	} else {
		fmt.Fprintln(out, "token: $"+config.WorkspaceTokenEnv)
		fmt.Fprintln(out, "connect: "+config.WorkspaceTokenEnv+"=... "+render.Clean(w.name)+" -workspace "+render.Clean(url))
	}
	fmt.Fprintln(out, `or in your config: {"workspace": {"url": "`+render.Clean(url)+`", "token_env": "`+config.WorkspaceTokenEnv+`"`+ca+`}}`)
	if w.ready != nil {
		w.ready(url)
	}

	stop := ctx
	if w.signals {
		var cancel context.CancelFunc
		stop, cancel = signal.NotifyContext(ctx, stopSignals...)
		defer cancel()
	}
	var serveErr error
	select {
	case <-stop.Done():
	case serveErr = <-served:
	}
	// A second signal while stopping cuts the waits short: run returns
	// at once, and its deferred cleanup still removes the token file.
	again := make(chan os.Signal, 1)
	if w.signals {
		signal.Notify(again, stopSignals...)
		defer signal.Stop(again)
	}
	drain := w.drain
	if drain == 0 {
		drain = 3 * time.Second
	}
	// Every session's processes and every Exec end first, which ends the
	// streams reading them. An Exec whose client stopped reading is
	// blocked writing its output to the connection, which only closing
	// the connections ends: the handler's close is given drain, then the
	// connections are closed, and it is given drain again.
	closed := make(chan struct{})
	go func() { h.Close(); close(closed) }()
	hurried := false
	wait := func() bool {
		if hurried {
			return false
		}
		t := time.NewTimer(drain)
		defer t.Stop()
		select {
		case <-closed:
			return true
		case <-t.C:
		case <-again:
			hurried = true
		}
		return false
	}
	if wait() {
		sctx, cancel := context.WithTimeout(context.Background(), time.Second)
		srv.Shutdown(sctx)
		cancel()
		srv.Close()
	} else {
		srv.Close()
		if !wait() && !hurried {
			fmt.Fprintln(out, "dax workspace: stopped with a request still ending")
		}
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("workspace: %w", serveErr)
	}
	return nil
}

// listenWorkspace is where dax workspace serves: HTTPS on addr with
// cfg, which may then be any address, and HTTP on a loopback address
// otherwise. It returns the URL scheme.
func listenWorkspace(addr string, cfg *tls.Config) (net.Listener, string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, "", fmt.Errorf("workspace: -listen %q: %w", addr, err)
	}
	if cfg != nil {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, "", fmt.Errorf("workspace: -listen: %w", err)
		}
		return tls.NewListener(ln, cfg), "https", nil
	}
	if port == "" || port == "0" {
		port = "7779"
	}
	refuse := fmt.Errorf("workspace: -listen %q: without TLS dax workspace listens on loopback only (127.0.0.1, ::1 or localhost), since its token and the files would cross the network in the clear; from another machine, tunnel to it: ssh -L %s:127.0.0.1:%s <this host>, or serve with -tls-cert and -tls-key", addr, port, port)
	if !isLoopbackHost(host) {
		return nil, "", refuse
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", fmt.Errorf("workspace: -listen: %w", err)
	}
	if a, ok := ln.Addr().(*net.TCPAddr); !ok || !a.IP.IsLoopback() {
		ln.Close()
		return nil, "", refuse
	}
	return ln, "http", nil
}

// dialWorkspace dials the served workspace s names, with the token in
// its variable, which is sent as a bearer token and never printed: the
// workspace the session's tools act in. The variable is taken out of
// this process's environment once read, so no child here (an MCP
// server on this machine, the key command) inherits it, whatever its
// name. An https URL is verified against the system's roots, or
// s.TLSCA in their place; nothing skips the verification, and no
// redirect is followed. The settings checked the URL: https, or http to
// a loopback host.
func dialWorkspace(ctx context.Context, s config.WorkspaceSettings) (wsremote.Workspace, error) {
	token := os.Getenv(s.TokenEnv)
	if token == "" {
		return nil, fmt.Errorf("-workspace: no token: set $%s to the token dax workspace took or wrote (the file it printed)", s.TokenEnv)
	}
	os.Unsetenv(s.TokenEnv)
	hc, err := workspaceHTTPClient(s.URL, s.TLSCA)
	if err != nil {
		return nil, err
	}
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ws, err := wsremote.Dial(dctx, s.URL, wsremote.WithBearerToken(token), wsremote.WithHTTPClient(hc))
	if err != nil {
		return nil, fmt.Errorf("-workspace %s: %w", s.URL, err)
	}
	return ws, nil
}

// workspaceHTTPClient is the client -workspace reaches base with: no
// Timeout, which would cut the streams, no redirect followed, and for
// https TLS 1.2 at least, verified against the system's roots or the CA
// in caFile.
func workspaceHTTPClient(base, caFile string) (*http.Client, error) {
	u, err := neturl.Parse(base)
	if err != nil {
		return nil, errors.New("-workspace: the URL does not parse")
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	if u.Scheme == "https" {
		cfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if caFile != "" {
			pem, err := os.ReadFile(caFile)
			if err != nil {
				return nil, fmt.Errorf("workspace.tls_ca: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("workspace.tls_ca %s holds no PEM certificate", caFile)
			}
			cfg.RootCAs = pool
		}
		t.TLSClientConfig = cfg
	}
	return &http.Client{
		Transport:     t,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// workspaceLine is the banner's line about a served workspace: where it
// is and its root, and when it cannot start a process (its token lacks
// exec, so bash fails too), that the session's MCP servers run here.
func workspaceLine(ws workspace.Workspace) string {
	d := ws.Descriptor()
	line := d.Kind + " " + d.Ref + " · " + d.Root
	if _, ok := ws.(workspace.Starter); !ok {
		line += " · starts no process there: its MCP servers run here"
	}
	return line
}
