package dax

// This file is dax attach: the terminal client over a session dax
// serve serves (serve.go). The view is the same as the local one's:
// the record (agentsession's remote client, which verifies every entry
// it reads) and the session's events (front/control's client) in, and
// what the user asks out as intents, through client.Direct, to the
// served session, whose own rules decide them; the head move, which
// agentturn's contract does not carry, goes through the served
// continue-from command. A slash line runs dax's command as it does
// locally, through the served commands.
//
// Every view attached to a session drives the same conversation; there
// is no client of its own to own a run. Independent work forks.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agentconsole/client"
	"github.com/ChristopherDavenport/agentconsole/console"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/remote"
	"github.com/ChristopherDavenport/agentturn/front/control"

	"github.com/ChristopherDavenport/dax/agent"
	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/internal/authn"
)

// runAttach is dax attach, a mode with flags of its own that reads no
// config and opens no session: the session is the server's.
func runAttach(ctx context.Context, args []string, p program) error {
	fs := flag.NewFlagSet(p.name+" attach", flag.ContinueOnError)
	tokenFile := fs.String("token-file", "", "file holding the token dax serve wrote; without it, $"+serveTokenEnv)
	pricingFile := fs.String("pricing-file", "", "JSON file of model prices for the session cost")
	caFile := fs.String("tls-ca", "", "for an https URL: the CA certificate (PEM) to verify dax serve's certificate against, in place of the system's")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: %s attach [-token-file file] [-tls-ca file] URL\n", p.name)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("attach: want the URL dax serve printed")
	}
	// The token goes with every request, so the URL is checked before
	// anything is sent.
	hc, err := attachHTTPClient(fs.Arg(0), *caFile)
	if err != nil {
		return err
	}
	token, err := attachToken(*tokenFile)
	if err != nil {
		return err
	}
	if !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		return errors.New("attach: the terminal client needs a terminal for standard input and output")
	}
	cost, err := pricing(*pricingFile)
	if err != nil {
		return err
	}
	// The view draws dax's tools, and the program's own, as the local
	// client does; the extensions are only asked for their renderers.
	exts, err := p.selected(choice{Agents: true})
	if err != nil {
		return err
	}
	return (&attachFront{name: p.name, exts: exts, cost: cost, hc: hc}).Run(ctx, fs.Arg(0), token)
}

// attachHTTPClient is the client dax attach reaches base with. An https
// URL is verified against the system's roots, or the CA in caFile in
// their place; nothing skips the verification. An http URL is taken
// only to a loopback host, since the token would otherwise cross the
// network in the clear.
func attachHTTPClient(base, caFile string) (*http.Client, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("attach: %w", err)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if caFile != "" {
			return nil, errors.New("attach: -tls-ca is for an https URL")
		}
		if !isLoopbackHost(u.Hostname()) {
			return nil, fmt.Errorf("attach: %s is not loopback, and http would send the token in the clear: serve with -tls-cert and -tls-key and attach over https, or tunnel (ssh -L) and attach to 127.0.0.1", u.Host)
		}
		return http.DefaultClient, nil
	default:
		return nil, fmt.Errorf("attach: %q: want the http or https URL dax serve printed", base)
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("attach: -tls-ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("attach: -tls-ca %s holds no PEM certificate", caFile)
		}
		cfg.RootCAs = pool
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = cfg
	return &http.Client{Transport: t}, nil
}

// attachToken is the token to send: the file's, or the environment's.
func attachToken(file string) (string, error) {
	if file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("attach: -token-file: %w", err)
		}
		t := strings.TrimSpace(string(data))
		if t == "" {
			return "", fmt.Errorf("attach: -token-file %s is empty", file)
		}
		return t, nil
	}
	if t := os.Getenv(serveTokenEnv); t != "" {
		return t, nil
	}
	return "", fmt.Errorf("attach: no token: give -token-file, the file dax serve printed, or set $%s", serveTokenEnv)
}

// attachFront is the terminal client attached to a served session.
type attachFront struct {
	name string
	exts []extension.Extension
	cost client.Cost
	// hc reaches the server (attachHTTPClient); nil is http.DefaultClient.
	hc *http.Client
	// console are the options console.Run is given; tests add the pipe.
	console []console.Option
	// out is where the resume command goes at exit; nil is standard
	// output.
	out io.Writer
	// run is console.Run; tests replace it.
	run consoleRun
}

// Run attaches to the session served at base until the user leaves.
func (f *attachFront) Run(ctx context.Context, base, token string) error {
	base = strings.TrimSuffix(base, "/")
	hc := f.hc
	if hc == nil {
		hc = http.DefaultClient
	}
	// The token goes with every request to the served origin, from
	// dax's own client, and nowhere else.
	hc, err := authn.Client(hc, base, token)
	if err != nil {
		return fmt.Errorf("attach: %w", err)
	}
	ctl, err := control.Dial(ctx, base+controlPrefix, control.WithHTTPClient(hc))
	if err != nil {
		return fmt.Errorf("attach: %w", err)
	}
	store, err := remote.Dial(base+storePrefix, remote.WithHTTPClient(hc))
	if err != nil {
		return fmt.Errorf("attach: %w", err)
	}
	controls := &remoteControls{c: ctl}
	served, err := controls.fetch(ctx)
	if err != nil {
		return fmt.Errorf("attach: %w", err)
	}
	if served.Info.ID == "" {
		return errors.New("attach: the server names no session")
	}
	renderers, err := extension.Renderers(served.Dir, f.exts)
	if err != nil {
		return err
	}
	// The session's run is not the view's to wait for when it leaves:
	// the run goes on, served, and the record keeps it.
	opts := append([]console.Option{
		console.WithToolRenderers(renderers),
		console.WithDrainTimeout(0),
		console.WithWarn(func(string) {}),
	}, f.console...)
	if f.cost != nil {
		opts = append(opts, console.WithCost(f.cost))
	}
	rec := &remoteRecord{store: store, controls: controls}
	err = runConsole(ctx, f.run, rec, ctl, attachController(ctl, controls), ctl, controls, opts)
	out := f.out
	if out == nil {
		out = os.Stdout
	}
	fmt.Fprintln(out, resumeLine(f.name, served.Info.ID))
	return err
}

// attachController is the attached view's controller: the session's
// own rules decide every intent, through client.Direct on control's
// client, except the head move, which agentturn's contract does not
// carry and the served session offers as a command. A move the session
// refuses (a run going, an entry that leaves calls unanswered) comes
// back as the command's error, which the view shows as it does locally.
func attachController(ctl *control.Client, controls *remoteControls) client.Controller {
	act := detaching(client.Direct(ctl))
	return func(ctx context.Context, in client.Intent) error {
		if m, ok := in.(client.ContinueFrom); ok {
			return controls.ContinueFrom(ctx, m.EntryID)
		}
		return act(ctx, in)
	}
}

// detaching is act for a view that may leave while the session's run
// goes on: leaving an attached view is not stopping the session, whose
// run other views, or a later one, go on following. Two things would
// stop it, and neither is the user's ask:
//   - Direct runs a prompt or an answer on control's client under the
//     view's context, and that client sends an abort when its caller's
//     context ends, so the run is started under one the view's leaving
//     does not end;
//   - the terminal client sends an abort as it exits, the only one sent
//     once the view's context is done (the user left with /quit), which
//     is dropped. An abort the user asks for while the view is up
//     (ctrl+c) goes through.
func detaching(act client.Controller) client.Controller {
	return func(ctx context.Context, in client.Intent) error {
		switch in.(type) {
		case client.Abort:
			if ctx.Err() != nil {
				return nil
			}
		case client.Prompt, client.Answer:
			ctx = context.WithoutCancel(ctx)
		}
		return act(ctx, in)
	}
}

// remoteRecord is the served session's record, read through the store
// protocol. The session it follows is the one the server names when
// asked, as the local record follows the recorder's.
type remoteRecord struct {
	store    *remote.Client
	controls *remoteControls
}

func (r *remoteRecord) id(ctx context.Context) (string, error) {
	served, err := r.controls.fetch(ctx)
	if err != nil {
		return "", err
	}
	return served.Info.ID, nil
}

func (r *remoteRecord) Follow(ctx context.Context, from agentsession.Cursor) iter.Seq2[agentsession.Change, error] {
	id, err := r.id(ctx)
	if err != nil {
		return func(yield func(agentsession.Change, error) bool) { yield(agentsession.Change{}, err) }
	}
	return r.store.Follow(ctx, id, from)
}

// Verified is true: the client checks every entry it reads against its
// hash, so what the view shows is the record the session wrote.
func (*remoteRecord) Verified() bool { return true }

func (r *remoteRecord) Read(ctx context.Context, id string) (*agentsession.Session, error) {
	if id == "" {
		var err error
		if id, err = r.id(ctx); err != nil {
			return nil, err
		}
	}
	return r.store.Read(ctx, id)
}

// Refs lists the refs whose target is the session.
func (r *remoteRecord) Refs(ctx context.Context, id string) ([]agentsession.Ref, error) {
	if id == "" {
		var err error
		if id, err = r.id(ctx); err != nil {
			return nil, err
		}
	}
	var out []agentsession.Ref
	for ref, err := range r.store.ListRefs(ctx, "") {
		if err != nil {
			return nil, err
		}
		if ref.Target.Session == id {
			out = append(out, ref)
		}
	}
	return out, nil
}

// remoteControls is agent.Controls over the served commands, so the
// slash commands run against a served session as against a local one.
type remoteControls struct {
	c    *control.Client
	mu   sync.Mutex
	last servedInfo
}

var _ agent.Controls = (*remoteControls)(nil)

// commandTimeout bounds a command whose caller gives no context.
const commandTimeout = 30 * time.Second

func (r *remoteControls) fetch(ctx context.Context) (servedInfo, error) {
	raw, err := r.c.Command(ctx, cmdInfo, nil)
	if err != nil {
		return servedInfo{}, err
	}
	var s servedInfo
	if err := json.Unmarshal(raw, &s); err != nil {
		return servedInfo{}, fmt.Errorf("info: %w", err)
	}
	r.mu.Lock()
	r.last = s
	r.mu.Unlock()
	return s, nil
}

func (r *remoteControls) call(ctx context.Context, name string, args any) (json.RawMessage, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	return r.c.Command(ctx, name, raw)
}

func (r *remoteControls) SetModel(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	_, err := r.call(ctx, cmdModel, modelArgs{Name: name})
	return err
}

func (r *remoteControls) SetThink(on bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	_, err := r.call(ctx, cmdThink, thinkArgs{On: on})
	return err
}

func (r *remoteControls) AddMCP(ctx context.Context, name, command string) (string, error) {
	raw, err := r.call(ctx, cmdMCPAdd, mcpAddArgs{Name: name, Command: command})
	if err != nil {
		return "", err
	}
	var l mcpLabel
	if err := json.Unmarshal(raw, &l); err != nil {
		return "", fmt.Errorf("mcp add: %w", err)
	}
	return l.Label, nil
}

func (r *remoteControls) RemoveMCP(label string) error {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	_, err := r.call(ctx, cmdMCPRemove, mcpLabel{Label: label})
	return err
}

// ContinueFrom moves the served session's head to the entry.
func (r *remoteControls) ContinueFrom(ctx context.Context, entryID string) error {
	_, err := r.call(ctx, cmdContinueFrom, continueArgs{Entry: entryID})
	return err
}

// Info asks the server; if it cannot be asked, the last answer stands.
func (r *remoteControls) Info() agent.Info {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	if s, err := r.fetch(ctx); err == nil {
		return s.Info
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last.Info
}
