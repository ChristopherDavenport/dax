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
// listener is on loopback only: from another machine, tunnel to it.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ChristopherDavenport/agentsession/remote"
	"github.com/ChristopherDavenport/agentturn/front/control"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/internal/private"
	"github.com/ChristopherDavenport/dax/internal/render"
)

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
	// addr is where to listen; it must be a loopback address.
	addr string
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
	ln, err := listenLoopback(f.addr)
	if err != nil {
		return err
	}
	defer ln.Close()
	token, tokenFile, cleanup, err := serveToken(si.ID, f.tokenDir)
	if err != nil {
		return err
	}
	defer cleanup()
	h, closeStore := serveHandler(sess, f.info.Dir, token, func(err error) {
		fmt.Fprintln(out, render.Clean("dax serve: "+err.Error()))
	})
	// Questions and held calls wait for whoever attaches, for as long
	// as the session is served.
	defer sess.Answering()()

	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	url := "http://" + ln.Addr().String()
	for _, l := range banner(f.info, si) {
		fmt.Fprintln(out, render.Clean(l))
	}
	fmt.Fprintln(out, "serving at "+url)
	if tokenFile != "" {
		fmt.Fprintln(out, "token: "+render.Clean(tokenFile))
		fmt.Fprintln(out, "attach: "+render.Clean(f.info.Name)+" attach -token-file "+render.Clean(tokenFile)+" "+url)
	} else {
		fmt.Fprintln(out, "token: $"+serveTokenEnv)
		fmt.Fprintln(out, "attach: "+serveTokenEnv+"=... "+render.Clean(f.info.Name)+" attach "+url)
	}
	if f.ready != nil {
		f.ready(url)
	}

	stop := ctx
	if f.signals {
		var cancel context.CancelFunc
		stop, cancel = signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer cancel()
	}
	var serveErr error
	select {
	case <-stop.Done():
	case serveErr = <-served:
	}

	// A run still going is aborted and given time to write its end, so
	// the record says how it stopped; then the follows end and the
	// listener closes.
	if sess.State().Running {
		sess.Abort()
		drain := f.drain
		if drain == 0 {
			drain = 3 * time.Second
		}
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
	refusal = fmt.Errorf("-listen %q: dax serve listens on loopback only (127.0.0.1, ::1 or localhost); from another machine, tunnel to it: ssh -L %s:127.0.0.1:%s <this host>", addr, port, port)
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
	if t := os.Getenv(serveTokenEnv); t != "" {
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
	file = filepath.Join(dir, id+".token")
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
// record under storePrefix, both behind token. closeStore ends the
// record's follows.
func serveHandler(sess *agent.Session, dir, token string, logf func(error)) (h http.Handler, closeStore func()) {
	ctl := control.Handler(sess,
		control.WithAuthenticator(control.BearerTokens(map[string]control.Principal{
			token: {Name: "attach", Scopes: control.AllScopes},
		})),
		control.WithCommands(serveCommands(sess, dir)),
	)
	store := remote.NewHandler(sess.Recorder().Store(),
		remote.WithAuthenticator(remote.BearerTokens(map[string]remote.Principal{
			token: {Name: "attach", Scopes: []remote.Scope{remote.ScopeRead}},
		})),
		remote.WithErrorLog(logf),
	)
	mux := http.NewServeMux()
	mux.Handle(controlPrefix+"/", http.StripPrefix(controlPrefix, ctl))
	mux.Handle(storePrefix+"/", http.StripPrefix(storePrefix, store))
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

// serveCommands are ctl's operations as front/control's named
// commands.
func serveCommands(ctl agent.Controls, dir string) map[string]control.Command {
	decode := func(raw json.RawMessage, v any) error {
		if len(raw) == 0 {
			return errors.New("arguments required")
		}
		return json.Unmarshal(raw, v)
	}
	return map[string]control.Command{
		cmdInfo: {
			Description: "the session's ID, where it is kept, its tools and what was left out, and the directory its tools act in",
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
