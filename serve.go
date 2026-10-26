package dax

// This file is dax serve: a session with no front of its own, its
// control and its record served over HTTP, for dax attach (attach.go)
// to drive from another terminal. The session is the one record and
// the one conversation: every view attached to it drives the same
// conversation, and independent work forks the session.
//
// The control is agentturn's front/control over the session (dax's
// agentturn.Control, its rules included), with dax's settings as named
// commands; the record is agentsession's remote, RFC 0003's read side,
// over the session's store. Both sit behind one bearer token, and the
// listener is on loopback only unless dax serve is given a certificate
// (-tls-cert, -tls-key): from another machine, tunnel to it, or serve
// HTTPS. With -a2a, agentturn's front/a2a is served beside them, behind
// a token of its own, each A2A conversation a session of its own
// (serve_a2a.go).

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ChristopherDavenport/agentsession/remote"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/front/control"
	"github.com/a2aproject/a2a-go/a2asrv"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/internal/authn"
	"github.com/ChristopherDavenport/dax/internal/private"
	"github.com/ChristopherDavenport/dax/internal/render"
)

// stopSignals stop dax serve, dax execute and dax workspace cleanly:
// an interrupt, a termination, a hangup (the terminal or the ssh
// session it ran in went away) and a quit, each ending what they
// started before they exit.
var stopSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT}

// serveIdle is how long a server of dax's keeps a keep-alive
// connection no request is on; it never cuts a stream.
const serveIdle = 2 * time.Minute

// serveTokenEnv names the variable that holds the token dax serve
// takes and dax attach sends. Without it, dax serve makes one and
// writes it to a file only the user can read.
const serveTokenEnv = "DAX_SERVE_TOKEN"

// The path prefixes the two handlers are served under, on one listener.
const (
	controlPrefix = "/control"
	storePrefix   = "/store"
)

// serveFront is dax serve, a front that is none: it serves the session
// until it is told to stop.
type serveFront struct {
	info frontInfo
	// addr is where to listen: a loopback address, or any with TLS.
	addr string
	// tlsCert and tlsKey are the certificate and key files dax serve
	// serves HTTPS with; with neither it serves HTTP on loopback only.
	tlsCert, tlsKey string
	// out is where the start lines and the resume command go; nil is
	// standard output.
	out io.Writer
	// tokenDir is where a token made here is written; empty is
	// ~/.dax/serve.
	tokenDir string
	// signals stops serving on SIGINT and SIGTERM, besides the
	// context's end.
	signals bool
	// ready is told the base URL once the listener is up; tests set it.
	ready func(url string)
	// drain bounds the wait for an aborted run to end at shutdown.
	drain time.Duration
	// a2a mounts agentturn's front/a2a beside the control
	// (serve_a2a.go); a2aEscalate puts a call the policy asks about to
	// the caller; a2aURL, checked, is the base URL its card names. a2aDir
	// is where its conversations are kept; empty is under ~/.dax/a2a, by
	// the session store's root (sessions, the -sessions it was opened
	// from), the working directory dax serve was started in (hostDir)
	// and the workspace.
	a2a, a2aEscalate          bool
	a2aURL                    string
	a2aDir, sessions, hostDir string
}

// Prepare leaves the session's notes printed as they come: nothing
// else has the terminal.
func (f *serveFront) Prepare(*agent.Options) {}

func (f *serveFront) Run(ctx context.Context, sess *agent.Session) error {
	out := f.out
	if out == nil {
		out = os.Stdout
	}
	si := sess.Info()
	if !si.Recorded {
		return errors.New("dax serve needs a session store: an attached view follows the record; drop -sessions \"\"")
	}
	ln, scheme, err := f.listen()
	if err != nil {
		return err
	}
	defer ln.Close()
	token, tokenFile, cleanup, err := serveToken(si.ID, f.tokenDir)
	if err != nil {
		return err
	}
	defer cleanup()
	url := scheme + "://" + servedHost(ln.Addr())
	// The A2A conversations' runs end when serving does.
	peersCtx, endPeers := context.WithCancel(context.WithoutCancel(ctx))
	defer endPeers()
	var (
		peers                  *a2aPeers
		a2aToken, a2aTokenFile string
	)
	if f.a2a {
		// A2A has a token of its own: a caller lent the agent is not
		// lent the control, which answers the questions its
		// conversation asks and adds MCP servers, or the store.
		var cleanupA2A func()
		a2aToken, a2aTokenFile, cleanupA2A, err = makeToken(a2aTokenEnv, f.tokenDir, si.ID+".a2a.token")
		if err != nil {
			return err
		}
		defer cleanupA2A()
		if a2aToken == token {
			return errors.New("dax serve: $" + a2aTokenEnv + " is serve's token; A2A takes a token of its own")
		}
		var base *neturl.URL
		if f.a2aURL != "" {
			if base, err = checkA2AURL(f.a2aURL); err != nil {
				return err
			}
		}
		dir := f.a2aDir
		if dir == "" {
			dir = a2aDir(storeRoot(sess, f.sessions), f.hostDir, si.Workspace)
		}
		peers, err = newA2APeers(sess, a2aOptions{
			base: base, version: f.info.Version, dir: dir,
			tokens:   map[string]string{a2aToken: a2aPrincipal},
			escalate: f.a2aEscalate,
			stop:     peersCtx,
		})
		if err != nil {
			return err
		}
	}
	h, closeStore := serveHandler(sess, f.info.Dir, token, peers, func(err error) {
		fmt.Fprintln(out, render.Clean("dax serve: "+err.Error()))
	})
	// Questions and held calls wait for whoever attaches, for as long
	// as the session is served.
	defer sess.Answering()()

	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: serveIdle}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	for _, l := range banner(f.info, si) {
		fmt.Fprintln(out, render.Clean(l))
	}
	fmt.Fprintln(out, "serving at "+render.Clean(url))
	if peers != nil {
		at := url
		if f.a2aURL != "" {
			at = strings.TrimSuffix(f.a2aURL, "/")
		}
		line := "a2a: " + render.Clean(at+a2aPath) + ", card at " + render.Clean(at+a2asrv.WellKnownAgentCardPath)
		if f.a2aEscalate {
			line += "; the policy's questions go to the caller"
		}
		fmt.Fprintln(out, line)
		if a2aTokenFile != "" {
			fmt.Fprintln(out, "a2a token: "+render.Clean(a2aTokenFile))
		} else {
			fmt.Fprintln(out, "a2a token: $"+a2aTokenEnv)
		}
	}
	// A certificate the attaching machine does not trust is named to it
	// with -tls-ca.
	ca := ""
	if scheme == "https" {
		ca = "[-tls-ca <the certificate's CA>] "
	}
	if tokenFile != "" {
		fmt.Fprintln(out, "token: "+render.Clean(tokenFile))
		fmt.Fprintln(out, "attach: "+render.Clean(f.info.Name)+" attach "+ca+"-token-file "+render.Clean(tokenFile)+" "+render.Clean(url))
	} else {
		fmt.Fprintln(out, "token: $"+serveTokenEnv)
		fmt.Fprintln(out, "attach: "+serveTokenEnv+"=... "+render.Clean(f.info.Name)+" attach "+ca+render.Clean(url))
	}
	if f.ready != nil {
		f.ready(url)
	}

	stop := ctx
	if f.signals {
		var cancel context.CancelFunc
		stop, cancel = signal.NotifyContext(ctx, stopSignals...)
		defer cancel()
	}
	var serveErr error
	select {
	case <-stop.Done():
	case serveErr = <-served:
	}

	// A run still going is aborted and given time to write its end, so
	// the record says how it stopped, an A2A conversation's included;
	// then the follows end and the listener closes.
	drain := f.drain
	if drain == 0 {
		drain = 3 * time.Second
	}
	if peers != nil {
		peers.close(drain)
	}
	endPeers()
	if sess.State().Running {
		sess.Abort()
		for deadline := time.Now().Add(drain); sess.State().Running && time.Now().Before(deadline); {
			time.Sleep(20 * time.Millisecond)
		}
	}
	closeStore()
	sctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	srv.Shutdown(sctx)
	srv.Close()
	fmt.Fprintln(out, resumeLine(f.info.Name, si.ID))
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	return nil
}

// listenLoopback listens on addr, which must name a loopback address:
// the served session runs tools and answers what its policy asks, so
// it is not offered to the network. From another machine, a tunnel
// reaches it.
func listenLoopback(addr string) (net.Listener, error) {
	refuse, err := checkLoopback(addr)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	if a, ok := ln.Addr().(*net.TCPAddr); !ok || !a.IP.IsLoopback() {
		ln.Close()
		return nil, refuse
	}
	return ln, nil
}

// listen is where dax serve serves: HTTPS on addr when it was given a
// certificate, which may then be any address, and HTTP on a loopback
// address otherwise. It returns the URL scheme.
func (f *serveFront) listen() (net.Listener, string, error) {
	cfg, err := serveTLS(f.tlsCert, f.tlsKey)
	if err != nil {
		return nil, "", err
	}
	if cfg == nil {
		ln, err := listenLoopback(f.addr)
		return ln, "http", err
	}
	ln, err := net.Listen("tcp", f.addr)
	if err != nil {
		return nil, "", err
	}
	return tls.NewListener(ln, cfg), "https", nil
}

// serveTLS is the TLS dax serve serves with: the certificate and key
// files, both or neither (nil: no TLS). TLS 1.2 is the least it takes.
func serveTLS(certFile, keyFile string) (*tls.Config, error) {
	return loadTLS("dax serve", certFile, keyFile)
}

// loadTLS is the TLS mode serves with: the certificate and key files,
// both or neither (nil: no TLS), at TLS 1.2 at least.
func loadTLS(mode, certFile, keyFile string) (*tls.Config, error) {
	return loadTLSFiles(mode, "-tls-cert", "-tls-key", certFile, keyFile)
}

// loadTLSFiles is loadTLS with the certificate's and key's settings
// named as certName and keyName in its errors.
func loadTLSFiles(mode, certName, keyName, certFile, keyFile string) (*tls.Config, error) {
	switch {
	case certFile == "" && keyFile == "":
		return nil, nil
	case certFile == "" || keyFile == "":
		return nil, errors.New(mode + ": " + certName + " and " + keyName + " go together")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("%s: %s, %s: %w", mode, certName, keyName, err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

// servedHost is the host and port to attach to: the listener's address,
// or this machine's name in place of an address that names none
// (0.0.0.0, ::), which is what a certificate would name.
func servedHost(a net.Addr) string {
	t, ok := a.(*net.TCPAddr)
	if !ok || !t.IP.IsUnspecified() {
		return a.String()
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		return a.String()
	}
	return net.JoinHostPort(host, strconv.Itoa(t.Port))
}

// checkServeAddr refuses, before anything starts, what dax serve would
// refuse when it listens: an address that is not loopback without TLS,
// and one of -tls-cert and -tls-key without the other.
func checkServeAddr(addr, certFile, keyFile string) error {
	if (certFile == "") != (keyFile == "") {
		return errors.New("dax serve: -tls-cert and -tls-key go together")
	}
	if certFile != "" {
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return fmt.Errorf("-listen %q: %w", addr, err)
		}
		return nil
	}
	_, err := checkLoopback(addr)
	return err
}

// checkLoopback refuses an address that is not a loopback one, and
// returns the refusal for an address that resolves elsewhere once
// listened on.
func checkLoopback(addr string) (refusal error, err error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("-listen %q: %w", addr, err)
	}
	if port == "" || port == "0" {
		port = "7878"
	}
	refusal = fmt.Errorf("-listen %q: without TLS dax serve listens on loopback only (127.0.0.1, ::1 or localhost); from another machine, tunnel to it: ssh -L %s:127.0.0.1:%s <this host>, or serve with -tls-cert and -tls-key", addr, port, port)
	if !isLoopbackHost(host) {
		return nil, refusal
	}
	return refusal, nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// serveToken is the token the session is served behind: the
// environment's, or one made here and written to a file only the user
// can read, which cleanup removes. The token is never printed.
func serveToken(id, dir string) (token, file string, cleanup func(), err error) {
	return makeToken(serveTokenEnv, dir, id+".token")
}

// minTokenBytes is the least a token from the environment may be: a
// made one is 43 characters of 32 random bytes, and a short one a user
// chose (or one meant for something else) could be guessed.
const minTokenBytes = 32

// makeToken is a token: the one env holds, which must be at least
// minTokenBytes long, or one made here and written to name in dir
// (~/.dax/serve when empty), a file only the user can read, which
// cleanup removes.
func makeToken(env, dir, name string) (token, file string, cleanup func(), err error) {
	if t := os.Getenv(env); t != "" {
		if len(t) < minTokenBytes {
			return "", "", nil, fmt.Errorf("$%s is %d bytes; a token from the environment must be at least %d (openssl rand -base64 32 makes one), or leave it unset and one is made", env, len(t), minTokenBytes)
		}
		return t, "", func() {}, nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", nil, err
	}
	token = base64.RawURLEncoding.EncodeToString(buf)
	if dir == "" {
		dir = filepath.Join(agent.DefaultUserDir(), "serve")
	}
	if err := private.SecureDir(dir); err != nil {
		return "", "", nil, fmt.Errorf("serve token: %w", err)
	}
	file = filepath.Join(dir, name)
	fh, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", "", nil, fmt.Errorf("serve token: %w", err)
	}
	// A file left from an earlier serve keeps its mode through
	// O_TRUNC, so it is made private either way.
	if err := fh.Chmod(0o600); err != nil {
		fh.Close()
		return "", "", nil, fmt.Errorf("serve token: %w", err)
	}
	if _, err := fh.WriteString(token + "\n"); err != nil {
		fh.Close()
		return "", "", nil, fmt.Errorf("serve token: %w", err)
	}
	if err := fh.Close(); err != nil {
		return "", "", nil, fmt.Errorf("serve token: %w", err)
	}
	return token, file, func() { os.Remove(file) }, nil
}

// serveHandler serves the session's control under controlPrefix and its
// record under storePrefix, both behind token, and with peers front/a2a
// under a2aPath and its card, behind the A2A token peers takes, which
// is not token. closeStore ends the record's follows.
func serveHandler(sess *agent.Session, dir, token string, peers *a2aPeers, logf func(error)) (h http.Handler, closeStore func()) {
	ctl := control.Handler(viewed{sess},
		control.WithAuthenticator(authn.New(map[string]control.Principal{
			token: {Name: servePrincipal, Scopes: control.AllScopes},
		}).Authenticator(control.ErrUnauthenticated)),
		control.WithCommands(serveCommands(sess, sess, dir)),
	)
	store := remote.NewHandler(sess.Recorder().Store(),
		remote.WithAuthenticator(authn.New(map[string]remote.Principal{
			token: {Name: servePrincipal, Scopes: []remote.Scope{remote.ScopeRead}},
		}).Authenticator(remote.ErrUnauthenticated)),
		remote.WithErrorLog(logf),
	)
	mux := http.NewServeMux()
	mux.Handle(controlPrefix+"/", http.StripPrefix(controlPrefix, ctl))
	mux.Handle(storePrefix+"/", http.StripPrefix(storePrefix, store))
	if peers != nil {
		peers.mount(mux)
	}
	return mux, func() { store.Close() }
}

// servedInfo is what the info command answers: the session's Info and
// the directory its tools act in, which the attached view draws paths
// against.
type servedInfo struct {
	Info agent.Info `json:"info"`
	Dir  string     `json:"dir"`
}

// The commands dax serves, which are agent.Controls's operations: what
// the slash commands of an attached view run.
const (
	cmdInfo      = "info"
	cmdModel     = "model"
	cmdThink     = "think"
	cmdMCPAdd    = "mcp-add"
	cmdMCPRemove = "mcp-remove"
	// cmdContinueFrom moves the session's head: the tree of an attached
	// view continues from an entry, as a local view's does.
	cmdContinueFrom = "continue-from"
)

type modelArgs struct {
	Name string `json:"name"`
}

type thinkArgs struct {
	On bool `json:"on"`
}

type mcpAddArgs struct {
	Name    string `json:"name"`
	Command string `json:"command"`
}

type mcpLabel struct {
	Label string `json:"label"`
}

type continueArgs struct {
	Entry string `json:"entry"`
}

// headMover is the session's head move (agent.Session.ContinueFrom), the
// one control a view sends that is not agentturn's.
type headMover interface {
	ContinueFrom(ctx context.Context, entryID string) error
}

// serveCommands are ctl's operations, and head's move, as
// front/control's named commands.
func serveCommands(ctl agent.Controls, head headMover, dir string) map[string]control.Command {
	decode := func(raw json.RawMessage, v any) error {
		if len(raw) == 0 {
			return errors.New("arguments required")
		}
		return json.Unmarshal(raw, v)
	}
	return map[string]control.Command{
		cmdInfo: {
			Description: "the session's ID, where it is kept, its tools and what was left out, its MCP servers (where each runs and what it is for), and the directory its tools act in",
			Run: func(context.Context, json.RawMessage) (json.RawMessage, error) {
				return json.Marshal(servedInfo{Info: ctl.Info(), Dir: dir})
			},
		},
		cmdModel: {
			Description: "the model from the next request",
			Schema:      json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`),
			Run: func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
				var a modelArgs
				if err := decode(raw, &a); err != nil {
					return nil, err
				}
				if strings.TrimSpace(a.Name) == "" {
					return nil, errors.New("no model named")
				}
				return nil, ctl.SetModel(a.Name)
			},
		},
		cmdThink: {
			Description: "reasoning on or off from the next request",
			Schema:      json.RawMessage(`{"type":"object","properties":{"on":{"type":"boolean"}},"required":["on"]}`),
			Run: func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
				var a thinkArgs
				if err := decode(raw, &a); err != nil {
					return nil, err
				}
				return nil, ctl.SetThink(a.On)
			},
		},
		cmdMCPAdd: {
			Description: "start an MCP server; its tools from the next prompt",
			Schema:      json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"command":{"type":"string"}},"required":["name","command"]}`),
			Run: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
				var a mcpAddArgs
				if err := decode(raw, &a); err != nil {
					return nil, err
				}
				label, err := ctl.AddMCP(ctx, a.Name, a.Command)
				if err != nil {
					return nil, err
				}
				return json.Marshal(mcpLabel{Label: label})
			},
		},
		cmdContinueFrom: {
			Description: "move the session's head to an entry: the next prompt continues from there, on a branch",
			Schema:      json.RawMessage(`{"type":"object","properties":{"entry":{"type":"string"}},"required":["entry"]}`),
			Run: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
				var a continueArgs
				if err := decode(raw, &a); err != nil {
					return nil, err
				}
				if strings.TrimSpace(a.Entry) == "" {
					return nil, errors.New("no entry named")
				}
				return nil, head.ContinueFrom(ctx, a.Entry)
			},
		},
		cmdMCPRemove: {
			Description: "stop an MCP server",
			Schema:      json.RawMessage(`{"type":"object","properties":{"label":{"type":"string"}},"required":["label"]}`),
			Run: func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
				var a mcpLabel
				if err := decode(raw, &a); err != nil {
					return nil, err
				}
				return nil, ctl.RemoveMCP(a.Label)
			},
		},
	}
}

// viewed is the session as its control serves it: each follower of its
// events is a view attached while it follows, shown the A2A
// conversations' questions still waiting when it attaches
// (agent.Session.SubscribeView), which a question from one needs to be
// asked.
type viewed struct{ *agent.Session }

func (v viewed) Subscribe(fn func(context.Context, agentturn.Event) error) func() {
	return v.SubscribeView(fn)
}
