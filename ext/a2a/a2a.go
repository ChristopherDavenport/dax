// Package a2a is dax-a2a, the extension that offers the user's A2A
// peers as tools: each agent named in the user's config is one tool,
// agentturn's tools/a2a over a client of the agent's card, so a call
// sends the model's request to the remote agent as one task and returns
// its answer. It is built only from what package extension offers any
// extension.
//
// A peer's tool is control, not execution: it is added through the
// kit (Owns), so it runs where the session does, with the user's
// environment and the session's questions, and never in an executor,
// which reads no config. It offers nothing when no peer is configured.
//
// The relation decides what an abandoned call does. A subsession's task
// (the default) is the call's own work: when the call is given up, an
// abort say, tools/a2a sends tasks/cancel and returns without waiting.
// A peer's task outlives the call and is never cancelled. A remote
// agent's question during a call is put to whoever answers the
// session's questions, as a tool's own question is; with nobody to
// answer, the call ends with the question for the model.
//
// Nothing is allowed by a shipped rule: every call asks unless the
// user's config allows the peer by name.
package a2a

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ChristopherDavenport/agentconsole/toolview"
	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	a2atool "github.com/ChristopherDavenport/agentturn/tools/a2a"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2aclient"
	"github.com/a2aproject/a2a-go/a2aclient/agentcard"

	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/toolrender"
)

// Name is the extension's name, and so its rules' source:
// extension:dax-a2a.
const Name = "dax-a2a"

// The relations a peer may have to the session, as the session's
// record links its callee.
const (
	// Subsession is a remote agent whose task is the call's own work,
	// cancelled when the call is given up. It is the default.
	Subsession = a2atool.RelationSubsession
	// Peer is a remote agent whose work outlives the call and keeps its
	// own record; its task is never cancelled.
	Peer = a2atool.RelationPeer
)

// DefaultCardTimeout bounds the fetch of each peer's agent card when the
// session starts.
const DefaultCardTimeout = 5 * time.Second

// A Remote is one A2A agent the model may call, as the user's config
// names it.
type Remote struct {
	// Name is the tool's name, which the model calls and a rule names.
	// The card's own name never replaces it.
	Name string
	// URL is where the agent's card is: the agent's base URL, the card
	// read from /.well-known/agent-card.json below it. https, or http to
	// a loopback host only.
	URL string
	// Relation is Subsession (the default when empty) or Peer.
	Relation string
	// TokenEnv names the environment variable that holds the bearer
	// token sent to the agent, at URL's origin only; empty sends none.
	// The token is never printed, logged or written.
	TokenEnv string
}

// Options configure dax-a2a.
type Options struct {
	// Remotes are the agents offered, one tool each, in this order.
	Remotes []Remote
	// CardTimeout bounds each card's fetch; zero is DefaultCardTimeout.
	CardTimeout time.Duration

	// transport carries the clients' requests; nil is
	// http.DefaultTransport. Only this package's tests set it, to trust
	// a test server's certificate.
	transport http.RoundTripper
}

var (
	toolName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)
	envName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Check reports whether r can be offered: a tool name, a URL a token
// may be sent to, a known relation and a variable's name.
func (r Remote) Check() error {
	switch {
	case !toolName.MatchString(r.Name):
		return fmt.Errorf("peer name %q: want a letter, then letters, digits, - and _, at most 64", r.Name)
	case strings.HasPrefix(strings.ToLower(r.Name), "mcp__"):
		return fmt.Errorf("peer name %q: mcp__ names an MCP server's tools", r.Name)
	}
	if err := checkURL(r.URL); err != nil {
		return fmt.Errorf("peer %s: url: %w", r.Name, err)
	}
	switch r.Relation {
	case "", Subsession, Peer:
	default:
		return fmt.Errorf("peer %s: relation %q: want %s or %s", r.Name, r.Relation, Subsession, Peer)
	}
	if r.TokenEnv != "" && !envName.MatchString(r.TokenEnv) {
		return fmt.Errorf("peer %s: token_env: %q is not a variable name", r.Name, r.TokenEnv)
	}
	return nil
}

// checkURL refuses a URL a token or a task should not be sent to: one
// that is not https, unless it is http to this machine, and one that
// carries credentials, a query or a fragment.
func checkURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("%q: want an https:// URL", s)
	}
	if u.User != nil {
		return fmt.Errorf("%q carries credentials; name the variable that holds the token in token_env", u.Redacted())
	}
	if u.RawQuery != "" || u.Fragment != "" {
		// Not quoted: a query is where a key would be.
		return errors.New("want the agent's base URL, with no query or fragment")
	}
	if u.Scheme == "http" && !loopback(u.Hostname()) {
		return fmt.Errorf("%q: http only to a loopback host, since the request and its token would cross the network in the clear", s)
	}
	return nil
}

func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// New is dax-a2a over the remotes in o. It owns a tool per remote and
// ships no rule, so each call asks unless the user allows it. When the
// session starts, each remote's card is fetched, within o.CardTimeout;
// a remote whose card cannot be fetched, or whose token variable is not
// set, is not offered, and the session says why once.
func New(o Options) extension.Extension {
	e := extension.Extension{Name: Name}
	if len(o.Remotes) == 0 {
		return e
	}
	names := make([]string, len(o.Remotes))
	for i, r := range o.Remotes {
		names[i] = r.Name
	}
	e.Owns = names
	for _, r := range o.Remotes {
		if r.TokenEnv != "" {
			e.Secrets = append(e.Secrets, r.TokenEnv)
		}
	}
	e.Renderers = func(string) toolview.Renderers { return toolrender.Peers(names...) }
	e.Kit = func(env extension.Env) ([]agentkit.Option, error) {
		for _, r := range o.Remotes {
			if err := r.Check(); err != nil {
				return nil, fmt.Errorf("%s: %w", Name, err)
			}
		}
		timeout := o.CardTimeout
		if timeout <= 0 {
			timeout = DefaultCardTimeout
		}
		// The cards are fetched at once; what each found is said in
		// the config's order.
		tools := make([]agenttool.Tool, len(o.Remotes))
		errs := make([]error, len(o.Remotes))
		var wg sync.WaitGroup
		for i, r := range o.Remotes {
			wg.Go(func() { tools[i], errs[i] = connect(context.Background(), r, timeout, o.transport) })
		}
		wg.Wait()
		var offered []agenttool.Tool
		for i, t := range tools {
			if errs[i] != nil {
				env.Log("[peer %s not offered: %v]", o.Remotes[i].Name, errs[i])
				continue
			}
			offered = append(offered, t)
		}
		if len(offered) == 0 {
			return nil, nil
		}
		return []agentkit.Option{agentkit.WithTools(offered...)}, nil
	}
	return e
}

// connect fetches r's card and builds its tool over a client of the
// card's JSON-RPC endpoint, which must be at r's origin: the card, not
// the user, names it, so a card that sends the work elsewhere (another
// host, a port on this machine) is refused here and the peer is not
// offered. The client sends the token, refuses a redirect to another
// origin, and tells each call the endpoint it reached
// (ObserveEndpoint).
func connect(ctx context.Context, r Remote, timeout time.Duration, rt http.RoundTripper) (agenttool.Tool, error) {
	if rt == nil {
		rt = http.DefaultTransport
	}
	token := ""
	if r.TokenEnv != "" {
		if token = os.Getenv(r.TokenEnv); token == "" {
			return nil, fmt.Errorf("$%s, its token_env, is not set", r.TokenEnv)
		}
	}
	base, _ := url.Parse(r.URL) // checked
	g := guard{origin: origin(base), token: token}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var ropts []agentcard.ResolveOption
	if token != "" {
		ropts = append(ropts, agentcard.WithRequestHeader("Authorization", "Bearer "+token))
	}
	cardClient := &http.Client{Timeout: timeout, CheckRedirect: g.redirect, Transport: limitBody{rt: rt, n: maxCardBytes}}
	card, err := agentcard.NewResolver(cardClient).Resolve(ctx, r.URL, ropts...)
	if err != nil {
		return nil, fmt.Errorf("its agent card: %w", err)
	}
	endpoint, ok := jsonRPCEndpoint(card)
	if !ok {
		return nil, errors.New("its agent card names no JSON-RPC endpoint")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || origin(u) != g.origin || checkURL(endpointOf(u)) != nil {
		return nil, fmt.Errorf("its agent card names the endpoint %q, which is not at %s", endpoint, g.origin)
	}
	// The client is built over the one endpoint checked, whatever else
	// the card lists.
	one := *card
	one.URL, one.PreferredTransport, one.AdditionalInterfaces = endpoint, a2a.TransportProtocolJSONRPC, nil
	// The JSON-RPC client has no deadline of its own: a task runs as
	// long as its call does, and an abort ends the call.
	client, err := a2aclient.NewFromCard(ctx, &one,
		a2aclient.WithJSONRPCTransport(&http.Client{CheckRedirect: g.redirect, Transport: rt}),
		a2aclient.WithInterceptors(g, a2atool.ObserveEndpoint()))
	if err != nil {
		return nil, fmt.Errorf("its agent card: %w", err)
	}
	opts := []a2atool.Option{
		a2atool.WithName(r.Name),
		a2atool.WithDescription(describe(r, card)),
		a2atool.WithInputRequired(answerer(r)),
	}
	if r.Relation == Peer {
		opts = append(opts, a2atool.WithoutCancelOnAbort())
	}
	return exchanged{a2atool.New(client, &one, opts...)}, nil
}

// exchanged is a peer's tool whose output, or error, ends with the
// call's own exchange with the person: what the agent asked and what
// was answered in the person's name, so the model knows what the agent
// was told. It is the call's result, not a note: a note the person
// gave is said once, by the loop, in their voice.
type exchanged struct{ agenttool.Tool }

func (t exchanged) Execute(ctx context.Context, call agenttool.Call) (agenttool.Result, error) {
	x := &exchange{}
	res, err := t.Tool.Execute(context.WithValue(ctx, exchangeKey{}, x), call)
	said := x.text()
	switch {
	case said == "":
	case err != nil:
		err = fmt.Errorf("%w\n\n%s", err, said)
	case len(res.Output.Parts) > 0:
		res.Output.Parts = append(res.Output.Parts, &openresponses.Text{Text: "\n\n" + said})
	default:
		res.Output.Text += "\n\n" + said
	}
	return res, err
}

type exchangeKey struct{}

// exchange is what one call's answers said, in order.
type exchange struct {
	mu    sync.Mutex
	lines []string
}

func (x *exchange) add(line string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.lines = append(x.lines, line)
}

func (x *exchange) text() string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return strings.Join(x.lines, "\n")
}

// jsonRPCEndpoint is the URL of the first JSON-RPC interface the card
// names, in its order of preference: its own URL, whose transport is
// JSON-RPC when the card names none, then its additional interfaces.
func jsonRPCEndpoint(card *a2a.AgentCard) (string, bool) {
	if card.URL != "" && (card.PreferredTransport == "" || card.PreferredTransport == a2a.TransportProtocolJSONRPC) {
		return card.URL, true
	}
	for _, i := range card.AdditionalInterfaces {
		if i.Transport == a2a.TransportProtocolJSONRPC && i.URL != "" {
			return i.URL, true
		}
	}
	return "", false
}

// maxCardBytes bounds an agent card's body: a card is a few kilobytes,
// and the start of a session must not read a stream into memory.
const maxCardBytes = 1 << 20

// limitBody fails a response whose body runs past n bytes.
type limitBody struct {
	rt http.RoundTripper
	n  int64
}

func (l limitBody) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := l.rt.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = &capped{rc: resp.Body, left: l.n}
	return resp, nil
}

type capped struct {
	rc   io.ReadCloser
	left int64
}

func (c *capped) Read(p []byte) (int, error) {
	if c.left <= 0 {
		// One byte more tells a body at the bound from one past it.
		var b [1]byte
		if n, _ := c.rc.Read(b[:]); n > 0 {
			return 0, fmt.Errorf("the card is over %d bytes", maxCardBytes)
		}
		return 0, io.EOF
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.rc.Read(p)
	c.left -= int64(n)
	return n, err
}

func (c *capped) Close() error { return c.rc.Close() }

// guard is the client's interceptor: it refuses a request to any origin
// but the configured one, which connect checked the card's endpoint
// against, and adds the token.
type guard struct {
	a2aclient.PassthroughInterceptor
	origin, token string
}

func (g guard) Before(ctx context.Context, req *a2aclient.Request) (context.Context, error) {
	u, err := url.Parse(req.BaseURL)
	if err != nil || origin(u) != g.origin {
		return ctx, fmt.Errorf("the endpoint %q is not at %s", req.BaseURL, g.origin)
	}
	if g.token != "" {
		req.Meta.Append("Authorization", "Bearer "+g.token)
	}
	return ctx, nil
}

// redirect refuses a redirect to another origin, on the card's fetch
// and on a call alike. Go's client keeps the Authorization header on a
// redirect to a subdomain, to another port and from https to http, so
// a redirect to anywhere but the configured origin is not followed at
// all.
func (g guard) redirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if origin(req.URL) != g.origin {
		return fmt.Errorf("a redirect to %s, another origin than %s, is not followed", origin(req.URL), g.origin)
	}
	return nil
}

// endpointOf is u without its query and fragment, which an endpoint may
// carry where a configured URL may not.
func endpointOf(u *url.URL) string {
	c := *u
	c.RawQuery, c.ForceQuery, c.Fragment, c.RawFragment = "", false, "", ""
	return c.String()
}

// origin is u's scheme, host and port, the port filled in.
func origin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	return u.Scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// maxDescription bounds what the card's description and skills add to
// the tool's description, which the model reads on every request.
const maxDescription = 2000

// describe is the tool's description: which agent it is, by the URL the
// user configured, what an abandoned call does, then what the card says
// of itself.
func describe(r Remote, card *a2a.AgentCard) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Sends a request to the A2A agent at %s and returns its answer. ", r.URL)
	if r.Relation == Peer {
		b.WriteString("It is a peer: its work goes on if this call is abandoned.")
	} else {
		b.WriteString("Its task is stopped if this call is abandoned.")
	}
	var self strings.Builder
	self.WriteString(strings.TrimSpace(card.Description))
	for _, s := range card.Skills {
		self.WriteString("\n- ")
		self.WriteString(s.Name)
		if s.Description != "" {
			self.WriteString(": ")
			self.WriteString(s.Description)
		}
	}
	if s := strings.TrimSpace(self.String()); s != "" {
		b.WriteString("\nThe agent describes itself:\n")
		b.WriteString(clip(s, maxDescription))
	}
	return b.String()
}

// maxQuestion bounds the remote agent's question as the person is asked
// it.
const maxQuestion = 1000

// answerer puts a remote agent's question, asked while a call to r
// runs, to whoever answers the session's questions, through the
// elicitor on the call's context, as a tool's own question goes: a
// question for a line of text (extension.TextQuestion). An answer is
// sent on the same task. A refusal ends the call with the question,
// and the task is given up as an aborted call's is; so is a call
// aborted while the question waits. With nobody to ask, or a question
// not answered, the call ends input-required, the question for the
// model: nothing answers in the person's place.
func answerer(r Remote) a2atool.InputRequired {
	return func(ctx context.Context, _ a2atool.TaskInfo, q *a2a.Message) ([]a2a.Part, error) {
		elicit, ok := agenttool.ElicitorFrom(ctx)
		if !ok {
			return nil, agentturn.ErrInputRequired
		}
		question := questionOf(q)
		// Who asks comes first, then how to answer, and the remote's
		// text last and quoted, so it cannot pass for either.
		ask := extension.TextQuestion(fmt.Sprintf("the A2A agent %s (%s) needs an answer to go on: reply with it, or refuse. It says: %s", r.Name, r.URL, question))
		ans, err := elicit(ctx, ask)
		if cerr := ctx.Err(); cerr != nil {
			// Given up while the question waited: the task is stopped
			// as an aborted call's.
			return nil, cerr
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %v", agentturn.ErrInputRequired, err)
		}
		if reply, ok := extension.TextAnswer(ask, ans); ok {
			if x, ok := ctx.Value(exchangeKey{}).(*exchange); ok {
				x.add(fmt.Sprintf("%s asked %s and the user answered %s.", r.Name, question, strconv.Quote(reply)))
			}
			return []a2a.Part{a2a.TextPart{Text: reply}}, nil
		}
		if ans.Action == agenttool.ActionDecline {
			return nil, &DeclinedError{Peer: r.Name, Question: question, Note: ans.Note}
		}
		return nil, agentturn.ErrInputRequired
	}
}

// DeclinedError ends a call whose remote agent asked a question that was
// refused. The model reads the question; what the person said with the
// refusal, Note, reaches it once, in the person's own voice, as every
// note on a question does, and is not repeated here.
type DeclinedError struct {
	Peer, Question, Note string
}

func (e *DeclinedError) Error() string {
	return fmt.Sprintf("%s asked %s and the question was declined", e.Peer, e.Question)
}

// questionOf is the text of a remote agent's question, clipped and
// quoted, with a count of the parts that are not text.
func questionOf(q *a2a.Message) string {
	var b strings.Builder
	other := 0
	if q != nil {
		for _, p := range q.Parts {
			if t, ok := p.(a2a.TextPart); ok {
				b.WriteString(t.Text)
			} else {
				other++
			}
		}
	}
	s := strings.TrimSpace(b.String())
	if s == "" {
		return "nothing (an empty question)"
	}
	// Quoted, its newlines and control characters escaped, so the
	// remote's text reads as one quotation and redraws nothing.
	s = strconv.Quote(clip(s, maxQuestion))
	if other > 0 {
		s += " (and " + strconv.Itoa(other) + " parts that are not text)"
	}
	return s
}

// clip cuts s to n bytes on a rune boundary, ending in … when it cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
