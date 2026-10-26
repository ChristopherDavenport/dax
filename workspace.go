package dax

// This file is the served workspace, both ends: `dax workspace`, a
// directory on this machine served whole over HTTP (agentworkspace's
// remote), and -workspace, a session whose tools act in one. It is the
// primitive cut: every stat, read, write and process crosses the wire
// on its own, and the tools, their facts and the policy stay with the
// session. `dax execute` is the other cut, at the tool calls, where the
// tools run where they act. tunnel.go turns it around, for a workspace
// the session cannot dial: `dax workspace -connect` dials the session's
// hub (workspace.hub).

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
	"maps"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"time"

	workspace "github.com/ChristopherDavenport/agentworkspace"
	wsremote "github.com/ChristopherDavenport/agentworkspace/remote"

	"github.com/ChristopherDavenport/dax/internal/authn"
	"github.com/ChristopherDavenport/dax/internal/config"
	"github.com/ChristopherDavenport/dax/internal/offer"
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
		fmt.Fprintf(fs.Output(), "usage: %s workspace [flags]\n\nserves a directory over HTTP as a workspace, for a session started elsewhere with -workspace to run its tools in, or with -connect through a session's hub (workspace.hub) it dials; reads no config file.\n", p.name)
		fs.PrintDefaults()
	}
	dir := fs.String("dir", "", "directory to serve; default the current directory")
	listen := fs.String("listen", workspaceListen, "address to serve at; a loopback address unless -tls-cert and -tls-key are given; port 0 picks one")
	passEnv := fs.String("pass-env", "", "comma-separated variables that look like credentials to pass to the workspace's processes anyway")
	tlsCert := fs.String("tls-cert", "", "serve HTTPS with this certificate (PEM), with -tls-key; then -listen may be any address")
	tlsKey := fs.String("tls-key", "", "the private key (PEM) of -tls-cert")
	connect := fs.String("connect", "", "in place of -listen, dial out to a session's hub (workspace.hub) at this wss:// URL, or ws:// to a loopback host, and serve the directory through that connection; the token from $"+config.TunnelTokenEnv)
	tlsCA := fs.String("tls-ca", "", "with a wss:// -connect, a CA certificate (PEM) to verify the hub's certificate against, in place of the system's roots")
	scopes := fs.String("scopes", "read,write,exec", "what the session may do here: a comma-separated list of read, write, exec and servers; servers is added to the default with -servers, and a server that names secret_env is offered only to a grant without exec and write")
	servers := fs.String("servers", "", `a file of MCP servers to offer the session, in dax's config shape, {"mcp_servers": {"<name>": {"command": "..."}}}: each a command run in the served directory, its secret_env given from this environment and redacted from its output`)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("workspace: unexpected argument %q", fs.Arg(0))
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	pass, err := splitPassEnv("workspace", *passEnv)
	if err != nil {
		return err
	}
	for _, env := range []string{config.WorkspaceTokenEnv, config.TunnelTokenEnv} {
		if slices.Contains(pass, env) {
			return fmt.Errorf("workspace: -pass-env: %s is this server's token, which no command it runs may be given", env)
		}
	}
	w := &workspaceServer{name: p.name, dir: *dir, addr: *listen, tlsCert: *tlsCert, tlsKey: *tlsKey, pass: pass, signals: true}
	if given["servers"] {
		if w.offered, err = readOffered(*servers); err != nil {
			return err
		}
		for _, o := range w.offered {
			for _, v := range o.SecretEnv {
				if slices.Contains(pass, v) {
					return fmt.Errorf("workspace: -pass-env: %s is server %s's secret (secret_env), which only that server is given", v, o.Name)
				}
			}
		}
	}
	if given["scopes"] {
		if w.scopes, err = parseScopes(*scopes); err != nil {
			return err
		}
	}
	// The grant is refused before a token is taken or anything served.
	if err := w.settle(); err != nil {
		return err
	}
	if !given["connect"] {
		if given["tls-ca"] {
			return errors.New("workspace: -tls-ca goes with -connect")
		}
		return w.run(ctx)
	}
	switch {
	case given["listen"]:
		return errors.New("workspace: -listen and -connect are exclusive: one serves at an address, the other dials a session's hub")
	case given["tls-cert"] || given["tls-key"]:
		return errors.New("workspace: -tls-cert and -tls-key are -listen's; with -connect the hub has the certificate, which -tls-ca verifies")
	}
	if err := checkConnectURL(*connect); err != nil {
		return err
	}
	if *tlsCA != "" && strings.HasPrefix(*connect, "ws://") {
		return errors.New("workspace: -tls-ca is for a wss:// hub; ws:// has no certificate to verify")
	}
	w.connect, w.tlsCA = *connect, *tlsCA
	// The token leaves the environment before the workspace is made,
	// whose processes inherit what is left of it.
	if w.token, err = takeTunnelToken("workspace -connect", config.TunnelTokenEnv); err != nil {
		return err
	}
	return w.run(ctx)
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
	// connect, in place of addr, is the hub it dials and serves the
	// workspace through (-connect), with token, verified against the
	// system's roots and tlsCA's certificate.
	connect, tlsCA string
	token          string
	// scopes are what the session may do, at addr or through the hub
	// (-scopes); nil is the default (settle).
	scopes []wsremote.Scope
	// offered are the MCP servers the workspace offers (-servers).
	offered []offer.Server
}

// readOffered is -servers: the file's servers, as config.ParseOffered
// checks them, in name order.
func readOffered(file string) ([]offer.Server, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("workspace: -servers: %w", err)
	}
	servers, err := config.ParseOffered(data, file)
	if err != nil {
		return nil, fmt.Errorf("workspace: -servers: %w", err)
	}
	var out []offer.Server
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		m := servers[name]
		out = append(out, offer.Server{Name: name, Description: m.Description, Command: m.Command, SecretEnv: m.SecretEnv})
	}
	return out, nil
}

// settle fills in the default grant, read, write and exec, and servers
// too when the workspace offers any, and refuses a grant that would
// give a secret away or that does not match what is offered: servers
// granted with nothing offered, or offered and not granted; write or
// exec without read, under which the session would act on files whose
// project rules it cannot read; and a secret holder offered beside
// another server, which runs as its user beside it. A server
// that holds secrets (secret_env) is offered only to a grant of servers
// alone: with exec the session reads a running server's environment
// through /proc, with write it changes the server's code, config or
// PATH in the directory to print its secret past the filter, and with
// read it reads what the server keeps in the directory, and /proc when
// the directory is / (agentworkspace's remote.ScopeServers says why).
func (w *workspaceServer) settle() error {
	if w.scopes == nil {
		w.scopes = []wsremote.Scope{wsremote.ScopeRead, wsremote.ScopeWrite, wsremote.ScopeExec}
		if len(w.offered) > 0 {
			w.scopes = append(w.scopes, wsremote.ScopeServers)
		}
	}
	grants := slices.Contains(w.scopes, wsremote.ScopeServers)
	reads := slices.Contains(w.scopes, wsremote.ScopeRead)
	for _, sc := range []wsremote.Scope{wsremote.ScopeWrite, wsremote.ScopeExec} {
		if slices.Contains(w.scopes, sc) && !reads {
			// The session would act on the project unable to read its
			// config, whose rules would then not apply.
			return fmt.Errorf("workspace: -scopes: %s without read: the session would act on files it may not read, its project's rules among them; add read", sc)
		}
	}
	switch {
	case grants && len(w.offered) == 0:
		return errors.New("workspace: -scopes servers: the workspace offers none; name them with -servers FILE")
	case !grants && len(w.offered) > 0:
		return errors.New("workspace: -servers: -scopes does not grant servers, so no session could start them; add servers to -scopes")
	}
	for _, o := range w.offered {
		if len(o.SecretEnv) == 0 {
			continue
		}
		if len(w.offered) > 1 {
			// Every server runs as the same user, in the same directory:
			// one that runs commands or writes files would hand the
			// session the secret through /proc or the holder's code.
			return fmt.Errorf("workspace: -servers: server %s holds secrets (secret_env), and a secret holder is offered alone, as the box host offers each unit in a container of its own: the others run beside it as its user, and one that runs a command or writes a file reads its secret; give it a -servers file and a dax workspace of its own", o.Name)
		}
		for _, sc := range []wsremote.Scope{wsremote.ScopeExec, wsremote.ScopeWrite, wsremote.ScopeRead} {
			if slices.Contains(w.scopes, sc) {
				return fmt.Errorf("workspace: server %s holds secrets (secret_env), which a session granted %s could take: with exec it reads the running server's environment through /proc, with write it changes the server's code or config to print them past the redaction, and with read it reads what the server keeps in the directory (and /proc too, with / served); offer a secret-holding server with -scopes servers alone, on a workspace of its own, as a session's servers_workspace", o.Name, sc)
			}
		}
	}
	return nil
}

// serversOnly reports whether the grant is the servers workspace's: the
// offered servers alone.
func (w *workspaceServer) serversOnly() bool {
	return len(w.scopes) == 1 && w.scopes[0] == wsremote.ScopeServers
}

// open is the workspace served: the directory as a workspace.Local
// whose processes get this machine's environment less its credentials
// and the offered servers' secrets, but those -pass-env names; with
// servers offered, an Offering over it (internal/offer), each server
// given its secret_env from this process's environment and its output
// redacted of their values here, before any of it is served.
func (w *workspaceServer) open() (workspace.Workspace, error) {
	dir := w.dir
	if dir == "" {
		var err error
		if dir, err = os.Getwd(); err != nil {
			return nil, err
		}
	}
	var secrets []string
	for _, o := range w.offered {
		secrets = append(secrets, o.SecretEnv...)
	}
	local, err := workspace.NewLocal(dir, tool.DefaultEnv(w.pass, secrets...))
	if err != nil {
		return nil, fmt.Errorf("workspace: %w", err)
	}
	if len(w.offered) == 0 {
		return local, nil
	}
	o, err := offer.Offer(local, w.offered, os.LookupEnv)
	if err != nil {
		local.Close()
		return nil, fmt.Errorf("workspace: -servers: %w", err)
	}
	return o, nil
}

// scopeList is the grant as the start line says it.
func scopeList(scopes []wsremote.Scope) string {
	names := make([]string, len(scopes))
	for i, s := range scopes {
		names[i] = string(s)
	}
	return strings.Join(names, ", ")
}

// sayOffered tells out the servers offered, by name and description,
// and the variables each is given as secrets, never their values.
func (w *workspaceServer) sayOffered(out io.Writer) {
	for _, o := range w.offered {
		line := "dax workspace: offers MCP server " + o.Name
		if o.Description != "" {
			line += ": " + o.Description
		}
		if len(o.SecretEnv) > 0 {
			line += " (given $" + strings.Join(o.SecretEnv, ", $") + ", redacted from its output)"
		}
		fmt.Fprintln(out, render.Clean(line))
	}
}

func (w *workspaceServer) run(ctx context.Context) error {
	if err := w.settle(); err != nil {
		return err
	}
	if w.connect != "" {
		return w.dial(ctx)
	}
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
	ws, err := w.open()
	if err != nil {
		return err
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
	h, err := wsremote.NewHandler(ws, wsremote.WithAuthenticator(authn.New(map[string]wsremote.Principal{
		token: {Name: workspacePrincipal, Scopes: w.scopes},
	}).Authenticator(wsremote.ErrUnauthenticated)))
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
	fmt.Fprintln(out, "dax workspace: serving "+render.Clean(ws.Root())+" at "+render.Clean(url)+", which may "+scopeList(w.scopes))
	w.sayOffered(out)
	ca := ""
	if scheme == "https" {
		ca = `, "tls_ca": "<the certificate's CA, unless the system trusts it>"`
	}
	// A grant of servers alone is for a session's servers
	// workspace, beside the workspace its tools act in.
	env, flag, field := config.WorkspaceTokenEnv, "-workspace", "workspace"
	if w.serversOnly() {
		env, flag, field = config.ServersWorkspaceTokenEnv, "-servers-workspace", config.ServersField
	}
	if tokenFile != "" {
		fmt.Fprintln(out, "token: "+render.Clean(tokenFile))
		fmt.Fprintln(out, "connect: "+env+"=\"$(cat "+render.Clean(tokenFile)+")\" "+render.Clean(w.name)+" "+flag+" "+render.Clean(url))
	} else {
		fmt.Fprintln(out, "token: $"+config.WorkspaceTokenEnv)
		fmt.Fprintln(out, "connect: "+env+"=... "+render.Clean(w.name)+" "+flag+" "+render.Clean(url))
	}
	fmt.Fprintln(out, `or in your config: {"`+field+`": {"url": "`+render.Clean(url)+`", "token_env": "`+env+`"`+ca+`}}`)
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

// takeWorkspaceToken is the served workspace's token, read from env and
// taken out of this process's environment at once, so no child here
// inherits it whatever its name: the run takes it as soon as its
// settings are resolved, before the key command or anything else
// starts a process.
func takeWorkspaceToken(flag, env string) (string, error) {
	token := os.Getenv(env)
	if token == "" {
		return "", fmt.Errorf("%s: no token: set $%s to the token dax workspace took or wrote (the file it printed)", flag, env)
	}
	os.Unsetenv(env)
	return token, nil
}

// dialWorkspace dials the served workspace s names with token
// (takeWorkspaceToken), which is sent as a bearer token and never
// printed: the workspace the session's tools act in. An https URL is
// verified against the system's roots, or s.TLSCA in their place;
// nothing skips the verification, and no redirect is followed. The
// settings checked the URL: https, or http to a loopback host.
func dialWorkspace(ctx context.Context, s config.WorkspaceSettings, token string) (wsremote.Workspace, error) {
	hc, err := workspaceHTTPClient(s.Name(), s.URL, s.TLSCA)
	if err != nil {
		return nil, err
	}
	// Every request can be given up at once (abandon), as a hub's are
	// when it is closed under the session.
	actx, abandon := context.WithCancel(context.Background())
	hc.Transport = abandonable{rt: hc.Transport, ctx: actx}
	// The token goes in the header of every request to the workspace's
	// origin, from dax's own client, and nowhere else.
	if hc, err = authn.Client(hc, s.URL, token); err != nil {
		abandon()
		return nil, fmt.Errorf("%s: %w", s.Flag(), err)
	}
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ws, err := wsremote.Dial(dctx, s.URL, wsremote.WithHTTPClient(hc))
	if err != nil {
		abandon()
		return nil, fmt.Errorf("%s %s: %w", s.Flag(), s.URL, err)
	}
	return identifyWith(ws, abandon), nil
}

// abandoner is a dialed workspace whose requests can all be given up at
// once, those in flight and any later: closeSession does so past its
// deadline, so a host that stopped answering (asleep, its network gone
// without a reset) does not hold the exit for remote's request timeout.
type abandoner interface{ Abandon() }

// abandonable is a RoundTripper whose every request, its body included,
// ends when ctx does.
type abandonable struct {
	rt  http.RoundTripper
	ctx context.Context
}

func (a abandonable) RoundTrip(r *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(a.ctx, cancel)
	done := func() { stop(); cancel() }
	resp, err := a.rt.RoundTrip(r.WithContext(ctx))
	if err != nil {
		done()
		return nil, err
	}
	resp.Body = &abandonableBody{ReadCloser: resp.Body, done: done}
	return resp, nil
}

// abandonableBody releases its request's ties to the abandon context
// when it is closed.
type abandonableBody struct {
	io.ReadCloser
	done func()
}

func (b *abandonableBody) Close() error {
	err := b.ReadCloser.Close()
	b.done()
	return err
}

// identify is ws under the identity dax records (config.WorkspaceRef):
// its descriptor's ref the URL normalised, so the record, -list, memory
// and the MCP trust take spellings of one address as one workspace. It
// hides nothing ws is: a Starter when ws is, a ServerProvider (the MCP
// servers the served workspace offers, under the servers scope) when ws
// is, both when ws is both, so each type assertion tells the truth; and
// its FS is ws's, an fs.ReadLinkFS when that is.
func identify(ws wsremote.Workspace) wsremote.Workspace { return identifyWith(ws, nil) }

// identifyWith is identify, its Abandon abandon (nil for none).
func identifyWith(ws wsremote.Workspace, abandon func()) wsremote.Workspace {
	w := identified{Workspace: ws, abandon: abandon}
	st, starts := ws.(workspace.Starter)
	sp, offers := ws.(workspace.ServerProvider)
	switch {
	case starts && offers:
		return identifiedStarterServers{identifiedStarter{w, st}, sp}
	case starts:
		return identifiedStarter{w, st}
	case offers:
		return identifiedServers{w, sp}
	}
	return w
}

type identified struct {
	wsremote.Workspace
	abandon func()
}

// Abandon gives up every request of the workspace's, in flight and
// later, when it was dialed (dialWorkspace); otherwise it does nothing.
func (w identified) Abandon() {
	if w.abandon != nil {
		w.abandon()
	}
}

// Close closes the workspace, then releases what Abandon would.
func (w identified) Close() error {
	err := w.Workspace.Close()
	w.Abandon()
	return err
}

func (w identified) Descriptor() workspace.Descriptor {
	d := w.Workspace.Descriptor()
	d.Ref = config.WorkspaceRef(d.Ref)
	return d
}

type identifiedStarter struct {
	identified
	st workspace.Starter
}

func (w identifiedStarter) Start(ctx context.Context, c workspace.Command) (workspace.Process, error) {
	return w.st.Start(ctx, c)
}

type identifiedServers struct {
	identified
	sp workspace.ServerProvider
}

func (w identifiedServers) Servers(ctx context.Context) ([]workspace.Server, error) {
	return w.sp.Servers(ctx)
}

func (w identifiedServers) StartServer(ctx context.Context, name string, stderr io.Writer) (workspace.Process, error) {
	return w.sp.StartServer(ctx, name, stderr)
}

type identifiedStarterServers struct {
	identifiedStarter
	sp workspace.ServerProvider
}

func (w identifiedStarterServers) Servers(ctx context.Context) ([]workspace.Server, error) {
	return w.sp.Servers(ctx)
}

func (w identifiedStarterServers) StartServer(ctx context.Context, name string, stderr io.Writer) (workspace.Process, error) {
	return w.sp.StartServer(ctx, name, stderr)
}

// workspaceHTTPClient is the client -workspace (or the setting field
// names, servers_workspace) reaches base with: no Timeout, which would
// cut the streams, no redirect followed, and for https TLS 1.2 at
// least, verified against the system's roots or the CA in caFile.
func workspaceHTTPClient(field, base, caFile string) (*http.Client, error) {
	u, err := neturl.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("%s: the URL does not parse", field)
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	if u.Scheme == "https" {
		cfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if caFile != "" {
			pem, err := os.ReadFile(caFile)
			if err != nil {
				return nil, fmt.Errorf("%s.tls_ca: %w", field, err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("%s.tls_ca %s holds no PEM certificate", field, caFile)
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
// exec, so bash fails too), that your MCP servers run here and the
// project's are refused.
func workspaceLine(ws workspace.Workspace) string {
	d := ws.Descriptor()
	return render.Clean(d.Kind+" "+d.Ref+" · "+d.Root) + startsNoProcess(ws)
}

// startsNoProcess is the banner's note on a served workspace that
// cannot start a process, and "" for one that can.
func startsNoProcess(ws workspace.Workspace) string {
	if _, ok := ws.(workspace.Starter); !ok {
		return " · starts no process there: your MCP servers run here, the project's not at all"
	}
	return ""
}
