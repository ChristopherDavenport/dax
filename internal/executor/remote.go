package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agenttool/mcpclient"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/agentworkspace/mcpworkspace"
	"github.com/ChristopherDavenport/agentworkspace/mcpworkspace/wire"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ChristopherDavenport/dax/tool"
)

// factsTimeout bounds one reading of a call's facts claim from an
// executor elsewhere. A reading that takes longer fails, and a failed
// reading blocks the call. A var for the tests.
var factsTimeout = 30 * time.Second

// redialTimeout bounds one reconnection to the executor: the start of
// its transport, the initialize and the listing. A var for the tests.
var redialTimeout = 30 * time.Second

// probeTimeout bounds the ping that asks, after a request failed with
// an error the executor did not answer with, whether the connection
// still stands. A var for the tests.
var probeTimeout = 5 * time.Second

// ConnectOptions name the session to the executor.
type ConnectOptions struct {
	// Name and Version are the client's implementation, the program's.
	Name, Version string
	// KeepAlive, when positive, is how often the client pings the
	// executor: over HTTP (KeepAlive), so the server does not close the
	// session as idle. A ping that fails closes the connection.
	KeepAlive time.Duration
	// Warn, when set, is told in a line that the connection was lost
	// and made again, or that the executor that answered was refused;
	// and, at the first connection, that its tools read less of a file
	// than MaxReadBytes asks (readLimitWarning).
	Warn func(string)
	// MaxReadBytes is the session's max_read_bytes, which the client
	// asks the executor for under CapabilityKey on every connection:
	// the executor's tools read up to it, capped at the executor's own
	// -max-read-bytes. Zero asks for tool.DefaultMaxRead.
	MaxReadBytes int64
	// Box, when set, is what the session asks of a box host (`dax box
	// serve`), beside MaxReadBytes under CapabilityKey: the profile and
	// its tightenings. An executor that is not a box host ignores it.
	Box json.RawMessage
}

// Dialer makes a new transport to the executor, for each connection a
// Remote makes: the first, and each one after the last was lost. A
// command's is a new process started as the first was; a URL's is a
// new client of the same endpoint.
type Dialer func(ctx context.Context) (sdk.Transport, error)

// Remote is the executor NewServer serves, reached over MCP: `dax
// execute` in a container or on another host, its tools' facts and
// replay claims read there, their calls run there, and their stamps
// made and checked there.
//
// A Remote made by Dial reconnects when its connection is lost (the
// process that served it exited, its session ended, a ping failed):
// the next request dials again before it is sent, so nothing that was
// never sent is lost. A call that was in flight when the connection
// went returns its error and is not sent again, since it may have run;
// a reading, which has no effect, is read again once on the new
// connection. The new connection must be the same executor: the same
// workspace, extensions and tools, each with the same definition and
// claims, as the first, since the model was offered those tools and the
// record names that workspace. One that is not is refused, and every
// request after fails, without dialing again.
type Remote struct {
	dial Dialer // nil: the first connection is the only one
	opts ConnectOptions
	// first is what the first connection said, which every later one
	// must say too.
	first shape

	ctx    context.Context // ended by Close, which stops a redial
	cancel context.CancelFunc
	// bg counts the redials and the closes of lost connections, which
	// Close waits for.
	bg sync.WaitGroup

	mu      sync.Mutex
	cur     *conn              // nil before a redial, or after one failed
	server  sdk.Implementation // the latest connection's
	attempt *attempt           // the redial in flight, which every request waits on
	refused error              // set when a redial found another executor
	closed  bool

	once     sync.Once
	closeErr error
}

// shape is what an executor says of itself that a session holds it to.
type shape struct {
	desc       workspace.Descriptor
	extensions []string
	tools      []Tool
	// maxRead is the read limit the executor gave the session's
	// tools; zero from an executor older than the field.
	maxRead int64
	// box is what a box host in front of it resolved; nil without one.
	box json.RawMessage
}

// conn is one connection to the executor and what hangs on it: the
// tools bound to its session, its processes' requests, and whether it
// is lost.
type conn struct {
	r      *Remote // nil until the Remote takes it
	c      *mcpclient.Remote
	server sdk.Implementation
	byName map[string]agenttool.Tool
	start  *wire.Bounds // nil when the executor starts no process
	// procs is the context of every request about a process that
	// waits on it (a read, a wait), ended before the connection
	// closes, which would otherwise wait for them; its cause says why.
	procs    context.Context
	endProcs context.CancelCauseFunc
	ended    chan struct{} // closed when the session ends
	lost     atomic.Bool   // a probe found it gone

	closeOnce sync.Once
	closeErr  error
}

// attempt is one redial, which the requests that wait on it share.
type attempt struct {
	done chan struct{}
	cn   *conn
	err  error
}

var _ Executor = (*Remote)(nil)

// The errors of a Remote whose connection is lost and not made again.
var (
	// ErrUnreachable is a request's when the connection was lost and
	// dialing again failed; the next request dials again.
	ErrUnreachable = errors.New("executor unreachable")
	// ErrChanged is every request's once a redial reached an executor
	// that is not the one the session started with.
	ErrChanged = errors.New("the executor changed")
	// errProcessLost is a process's when the connection it was started
	// over is lost: the executor ended it with that session.
	errProcessLost = errors.New("the connection the process was started over was lost, and the process ended with it; dax does not start it again")
	// errRemoteClosed is a request's after Close.
	errRemoteClosed = errors.New("executor: closed")
)

// Connect connects to the executor at the other end of t and checks
// that it is one: that it answers the facts method, and that its
// capability (CapabilityKey) is of a version this client reads and
// names every tool it lists, no more, each claiming facts exactly when
// the listed tool does. A server that fails any of these is refused,
// since a session that took its tools anyway would decide their calls
// on what the model wrote, not on what they would touch, which is a
// wider policy than the user's. The caller closes the Remote. A
// connection Connect makes is its only one: once it is lost, every
// request fails. Dial reconnects.
func Connect(ctx context.Context, t sdk.Transport, o ConnectOptions) (*Remote, error) {
	return connectWith(ctx, nil, t, o)
}

// Dial is Connect over a transport dial makes, and again over a new one
// whenever the connection is lost (see Remote).
func Dial(ctx context.Context, dial Dialer, o ConnectOptions) (*Remote, error) {
	t, err := dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("executor: %w", err)
	}
	return connectWith(ctx, dial, t, o)
}

func connectWith(ctx context.Context, dial Dialer, t sdk.Transport, o ConnectOptions) (*Remote, error) {
	cn, sh, err := connect(ctx, t, o)
	if err != nil {
		return nil, err
	}
	if line := readLimitWarning(o.MaxReadBytes, sh.maxRead, cn.server); line != "" && o.Warn != nil {
		o.Warn(line)
	}
	r := &Remote{dial: dial, opts: o, first: sh, cur: cn, server: cn.server}
	cn.r = r
	r.ctx, r.cancel = context.WithCancel(context.Background())
	return r, nil
}

// connect makes one connection over t and checks it (remoteOf).
func connect(ctx context.Context, t sdk.Transport, o ConnectOptions) (*conn, shape, error) {
	c, err := mcpclient.Connect(ctx, t,
		mcpclient.WithClaims(),
		mcpclient.WithElicitation(),
		mcpclient.WithClientInfo(o.Name, o.Version),
		mcpclient.WithClientOptions(sdk.ClientOptions{
			KeepAlive: o.KeepAlive,
			// What the session asks of the executor, the same on every
			// connection. The executor uses no other capability of a
			// client's but the elicitation mcpclient adds.
			Capabilities: &sdk.ClientCapabilities{Experimental: map[string]any{
				CapabilityKey: clientCapability{MaxReadBytes: o.MaxReadBytes, Box: o.Box},
			}},
		}),
		// The executor says its tool list never changes; a call does
		// not wait for a notification after it.
		mcpclient.WithNotificationGrace(0),
		// The process methods (mcpworkspace's), which only the
		// session sends.
		mcpclient.WithClientSetup(mcpworkspace.RegisterClient),
	)
	if err != nil {
		return nil, shape{}, fmt.Errorf("executor: %w", err)
	}
	cn, sh, err := remoteOf(c)
	if err != nil {
		c.Close()
		return nil, shape{}, err
	}
	cn.procs, cn.endProcs = context.WithCancelCause(context.Background())
	cn.ended = make(chan struct{})
	go func() {
		c.Session().Wait()
		close(cn.ended)
		// Whatever still waits on a process of the session is done.
		cn.endProcs(errProcessLost)
	}()
	return cn, sh, nil
}

// errNotExecutor is the start of every refusal of a server that does
// not say what a session needs of an executor.
var errNotExecutor = errors.New("not a dax executor")

// remoteOf checks what c's server said at initialize and listed, and
// builds the tools in the capability's order.
func remoteOf(c *mcpclient.Remote) (*conn, shape, error) {
	server := c.ServerInfo()
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("executor (%s %s): %w: %s", server.Name, server.Version, errNotExecutor, fmt.Sprintf(format, args...))
	}
	init := c.Session().InitializeResult()
	if init == nil || init.Capabilities == nil {
		return nil, shape{}, refuse("it gave no capabilities")
	}
	exp := init.Capabilities.Experimental
	if _, ok := exp[mcpclient.FactsCapability]; !ok {
		return nil, shape{}, refuse("it does not answer %s (no %s capability)", mcpclient.FactsMethod, mcpclient.FactsCapability)
	}
	v, ok := exp[CapabilityKey]
	if !ok {
		return nil, shape{}, refuse("no %s capability", CapabilityKey)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, shape{}, refuse("%s: %v", CapabilityKey, err)
	}
	var cp capability
	if err := json.Unmarshal(raw, &cp); err != nil {
		return nil, shape{}, refuse("%s: %v", CapabilityKey, err)
	}
	if cp.Version != CapabilityVersion {
		return nil, shape{}, refuse("%s version %d; this client reads version %d", CapabilityKey, cp.Version, CapabilityVersion)
	}
	if cp.Files == nil {
		return nil, shape{}, refuse("it does not serve its workspace's files (no files in %s), which the session reads AGENTS.md, .dax/skills and .dax/config.json through; update dax execute where it runs", CapabilityKey)
	}
	if cp.Files.URITemplate != wire.FilesURITemplate {
		return nil, shape{}, refuse("it serves its workspace's files under %q; this client reads %q", cp.Files.URITemplate, wire.FilesURITemplate)
	}
	if st := cp.Start; st != nil && !st.Valid() {
		return nil, shape{}, refuse("its start capability has a bound that is not positive: %+v", *st)
	}
	if cp.MaxReadBytes < 0 {
		return nil, shape{}, refuse("its read limit is %d bytes", cp.MaxReadBytes)
	}
	if cp.Descriptor.Kind == "" || cp.Descriptor.Root == "" {
		return nil, shape{}, refuse("its capability names no workspace (kind %q, root %q)", cp.Descriptor.Kind, cp.Descriptor.Root)
	}
	listed := map[string]agenttool.Tool{}
	for _, t := range c.Tools() {
		listed[t.Name()] = t
	}
	cn := &conn{c: c, server: server, byName: map[string]agenttool.Tool{}, start: cp.Start}
	sh := shape{
		desc:       workspace.Descriptor{Kind: cp.Descriptor.Kind, Ref: cp.Descriptor.Ref, Root: cp.Descriptor.Root},
		extensions: slices.Clone(cp.Extensions),
		maxRead:    cp.MaxReadBytes,
		box:        cp.Box,
	}
	for _, ct := range cp.Tools {
		t, ok := listed[ct.Name]
		switch {
		case ct.Name == "":
			return nil, shape{}, refuse("its capability names a tool with no name")
		case cn.byName[ct.Name] != nil:
			return nil, shape{}, refuse("its capability names %q twice", ct.Name)
		case !ok:
			return nil, shape{}, refuse("its capability names %q, which it does not list", ct.Name)
		case !slices.Contains(cp.Extensions, ct.Extension):
			return nil, shape{}, refuse("its capability says %q is of extension %q, which it does not name", ct.Name, ct.Extension)
		case ct.Facts && !agenttool.IsFactual(t):
			return nil, shape{}, refuse("its capability says %q claims its facts, but the tool is listed without the claim", ct.Name)
		case !ct.Facts && agenttool.IsFactual(t):
			return nil, shape{}, refuse("%q is listed with a facts claim its capability does not name", ct.Name)
		}
		tl := toolOf(ct.Extension, t, ct.ReadOnly)
		if ct.Strict {
			strict := true
			tl.Definition.Strict = &strict
		}
		sh.tools = append(sh.tools, tl)
		cn.byName[ct.Name] = t
	}
	for name := range listed {
		if cn.byName[name] == nil {
			return nil, shape{}, refuse("it lists %q, which its capability does not name", name)
		}
	}
	return cn, sh, nil
}

// differs says how o, a later connection's executor, is not s, the
// first's, and "" when it is the same: the workspace, the extensions,
// the read limit when both say one, and the tools in order, each with
// the same definition and claims. The same client asks every
// connection for the same read limit, so the same executor gives the
// same one; an executor older than the read limit gives none, and its
// tools' definitions, read's description among them, say what changed.
func (s shape) differs(o shape) string {
	if s.desc != o.desc {
		return fmt.Sprintf("it acts in %s, not %s", describe(o.desc), describe(s.desc))
	}
	if !slices.Equal(s.extensions, o.extensions) {
		return fmt.Sprintf("it runs the extensions %v, not %v", o.extensions, s.extensions)
	}
	if s.maxRead != 0 && o.maxRead != 0 && s.maxRead != o.maxRead {
		return fmt.Sprintf("its tools read up to %d bytes of a file, not %d (its -max-read-bytes changed)", o.maxRead, s.maxRead)
	}
	names := func(ts []Tool) []string {
		out := make([]string, len(ts))
		for i, t := range ts {
			out[i] = t.Name()
		}
		return out
	}
	if a, b := names(s.tools), names(o.tools); !slices.Equal(a, b) {
		return fmt.Sprintf("it runs the tools %v, not %v", b, a)
	}
	for i := range s.tools {
		a, errA := json.Marshal(s.tools[i])
		b, errB := json.Marshal(o.tools[i])
		if errA != nil || errB != nil || string(a) != string(b) {
			return fmt.Sprintf("its tool %q is not the one the session was offered (its definition or claims differ)", s.tools[i].Name())
		}
	}
	return ""
}

// readLimitWarning is what a session that asked for asked as its read
// limit (zero: tool.DefaultMaxRead) is told of got, the limit the
// executor server gave its tools (zero: an executor older than the
// limit, which says none): that it is lower than asked, or, from an
// older executor, that the session's setting does not reach it. ""
// when there is nothing to say.
func readLimitWarning(asked, got int64, server sdk.Implementation) string {
	want, setting := asked, fmt.Sprintf("max_read_bytes is %d", asked)
	if want <= 0 {
		want, setting = tool.DefaultMaxRead, fmt.Sprintf("max_read_bytes is the default, %d", tool.DefaultMaxRead)
	}
	switch {
	case got == 0 && want != tool.DefaultMaxRead:
		return fmt.Sprintf("%s, but the executor (%s %s) is older than the setting and does not take it: its tools read what its own -max-read-bytes says; update dax execute where it runs", setting, server.Name, server.Version)
	case got > 0 && got < want:
		return fmt.Sprintf("%s, but the executor (%s %s) caps it at %d bytes, so its tools read no more of a file than that; to allow more, pass `dax execute -max-read-bytes %d` where it runs", setting, server.Name, server.Version, got, want)
	}
	return ""
}

// describe is a workspace as a line names it.
func describe(d workspace.Descriptor) string {
	where := d.Kind
	if d.Ref != "" {
		where += " " + d.Ref
	}
	return where + " " + d.Root
}

// isLost says whether cn's session has ended or a probe found it gone.
func (cn *conn) isLost() bool {
	if cn.lost.Load() {
		return true
	}
	select {
	case <-cn.ended:
		return true
	default:
		return false
	}
}

// close ends cn's process requests, with cause, and then the
// connection, once.
func (cn *conn) close(cause error) error {
	cn.closeOnce.Do(func() {
		cn.endProcs(cause)
		cn.closeErr = cn.c.Close()
	})
	return cn.closeErr
}

// conn is the connection a request is sent on: the current one, or,
// when it is lost, a new one, dialed before anything is sent. Requests
// that find it lost together wait on one redial. A redial that fails is
// ErrUnreachable for the requests that waited on it, and the next
// request dials again; one that reaches another executor is ErrChanged
// for every request from then on.
func (r *Remote) conn(ctx context.Context) (*conn, error) {
	r.mu.Lock()
	switch {
	case r.closed:
		r.mu.Unlock()
		return nil, errRemoteClosed
	case r.refused != nil:
		err := r.refused
		r.mu.Unlock()
		return nil, err
	case r.cur != nil && !r.cur.isLost():
		cn := r.cur
		r.mu.Unlock()
		return cn, nil
	case r.dial == nil:
		r.mu.Unlock()
		return nil, fmt.Errorf("executor: the connection was lost; restart dax and resume the session (-resume): %w", ErrUnreachable)
	}
	a := r.attempt
	if a == nil {
		a = &attempt{done: make(chan struct{})}
		r.attempt = a
		old := r.cur
		r.cur = nil
		r.bg.Add(1)
		go r.redial(a, old)
	}
	r.mu.Unlock()
	select {
	case <-a.done:
		return a.cn, a.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// redial makes a new connection for a, within redialTimeout or until
// Close, while old, the lost one, closes.
func (r *Remote) redial(a *attempt, old *conn) {
	defer r.bg.Done()
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		if old != nil {
			old.close(errProcessLost)
		}
	}()
	defer func() { <-closed }()

	ctx, cancel := context.WithTimeout(r.ctx, redialTimeout)
	defer cancel()
	var cn *conn
	t, err := r.dial(ctx)
	if err == nil {
		var sh shape
		cn, sh, err = connect(ctx, t, r.opts)
		switch {
		case errors.Is(err, errNotExecutor):
			err = fmt.Errorf("%w: %w", ErrChanged, err)
		case err == nil:
			if why := r.first.differs(sh); why != "" {
				cn.close(errRemoteClosed)
				cn = nil
				err = fmt.Errorf("%w: %s", ErrChanged, why)
			}
		}
	}

	r.mu.Lock()
	r.attempt = nil
	switch {
	case r.closed:
		if cn != nil {
			cn.close(errRemoteClosed)
		}
		cn, err = nil, errRemoteClosed
	case errors.Is(err, ErrChanged):
		err = fmt.Errorf("executor: the connection was lost, and the executor that answered is not the one this session started with: %w; restart dax and resume the session (-resume)", err)
		r.refused = err
	case err != nil:
		err = fmt.Errorf("executor: the connection was lost, and reconnecting failed (%v); the next call will try again: %w", err, ErrUnreachable)
	default:
		cn.r = r
		r.cur, r.server = cn, cn.server
	}
	r.mu.Unlock()

	// Warned before the requests waiting on the attempt go on, so the
	// warning comes before anything they print.
	switch {
	case r.opts.Warn == nil:
	case cn != nil:
		r.opts.Warn(fmt.Sprintf("executor connection lost; reconnected to %s %s · %s", cn.server.Name, cn.server.Version, describe(r.first.desc)))
	case errors.Is(err, ErrChanged):
		r.opts.Warn(err.Error())
	}
	a.cn, a.err = cn, err
	close(a.done)
}

// lostUnder says whether err, a request's on cn that the caller's
// context did not end, came of the connection being lost: cn's session
// has ended, or, for an error the executor did not answer with, a ping
// fails. A connection a ping found gone is closed at once, so that the
// requests still waiting on it (a process's reads) end too.
func (r *Remote) lostUnder(cn *conn, err error) bool {
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	switch {
	case closed:
		// Close ended it; nothing was lost.
		return false
	case cn.isLost():
		return true
	}
	var answered *jsonrpc.Error
	if errors.As(err, &answered) {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	if cn.c.Session().Ping(ctx, nil) == nil {
		return false
	}
	cn.lost.Store(true)
	r.mu.Lock()
	if r.cur == cn && !r.closed {
		r.cur = nil
		r.bg.Add(1)
		go func() {
			defer r.bg.Done()
			cn.close(errProcessLost)
		}()
	}
	r.mu.Unlock()
	return true
}

// connErr says whether err is conn's own: the Remote closed, or its
// connection lost and not made again, which already says "executor".
func connErr(err error) bool {
	return errors.Is(err, errRemoteClosed) || errors.Is(err, ErrUnreachable) || errors.Is(err, ErrChanged)
}

// read sends a request that has no effect (a reading of facts or
// replay, a file) on the connection, and, when the connection was lost
// under it, once more on a new one. f returns the request's error only,
// not an answer's.
func (r *Remote) read(ctx context.Context, f func(*conn) error) error {
	for try := 0; ; try++ {
		cn, err := r.conn(ctx)
		if err != nil {
			return err
		}
		err = f(cn)
		if err == nil || try > 0 || ctx.Err() != nil || !r.lostUnder(cn, err) {
			return err
		}
	}
}

// Server is the name and version the executor gave at initialize, the
// latest connection's.
func (r *Remote) Server() (name, version string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.server.Name, r.server.Version
}

// Extensions are the extensions whose tools the executor runs, in its
// order, whether or not they built any.
func (r *Remote) Extensions() []string { return slices.Clone(r.first.extensions) }

func (r *Remote) Tools(context.Context) ([]Tool, error) { return slices.Clone(r.first.tools), nil }

func (cn *conn) tool(name string) (agenttool.Tool, error) {
	t, ok := cn.byName[name]
	if !ok {
		return nil, fmt.Errorf("no tool named %q", name)
	}
	return t, nil
}

// has says whether the executor runs a tool of that name.
func (r *Remote) has(name string) bool {
	return slices.ContainsFunc(r.first.tools, func(t Tool) bool { return t.Name() == name })
}

// Facts reads the call's claim in the executor, within factsTimeout.
// An error, the executor's, the connection's or the timeout's, is
// returned, and blocks the call.
func (r *Remote) Facts(ctx context.Context, c Call) (agenttool.Facts, error) {
	if !r.has(c.Tool) {
		return agenttool.Facts{}, fmt.Errorf("no tool named %q", c.Tool)
	}
	ctx, cancel := context.WithTimeout(ctx, factsTimeout)
	defer cancel()
	var ans mcpclient.FactsAnswer
	err := r.read(ctx, func(cn *conn) error {
		t, err := cn.tool(c.Tool)
		if err != nil {
			return err
		}
		if !agenttool.IsFactual(t) {
			ans = mcpclient.FactsAnswer{}
			return nil
		}
		got, err := cn.c.Facts(ctx, mcpclient.FactsCall{Name: c.Tool, Args: c.Args})
		if err != nil {
			return err
		}
		ans = got[0]
		return nil
	})
	if err == nil {
		err = ans.Err
	}
	switch {
	case err == nil:
		return ans.Facts, nil
	case connErr(err):
		return agenttool.Facts{}, err
	}
	return agenttool.Facts{}, fmt.Errorf("executor: %w", err)
}

var _ Batcher = (*Remote)(nil)

// BatchFacts reads the calls' claims in the executor in one request
// (mcpclient's Remote.Facts), within factsTimeout. A call's own error,
// a tool the executor does not have included, is in errs; the
// request's, the connection's or the timeout's, is err, and blocks
// every call of the batch.
func (r *Remote) BatchFacts(ctx context.Context, calls []Call) ([]agenttool.Facts, []error, error) {
	ask := make([]mcpclient.FactsCall, len(calls))
	for i, c := range calls {
		ask[i] = mcpclient.FactsCall{Name: c.Tool, Args: c.Args}
	}
	ctx, cancel := context.WithTimeout(ctx, factsTimeout)
	defer cancel()
	var got []mcpclient.FactsAnswer
	err := r.read(ctx, func(cn *conn) error {
		var err error
		got, err = cn.c.Facts(ctx, ask...)
		return err
	})
	switch {
	case err == nil:
	case connErr(err):
		return nil, nil, err
	default:
		return nil, nil, fmt.Errorf("executor: %w", err)
	}
	facts, errs := make([]agenttool.Facts, len(calls)), make([]error, len(calls))
	for i, a := range got {
		if a.Err != nil {
			errs[i] = fmt.Errorf("executor: %w", a.Err)
			continue
		}
		facts[i] = a.Facts
	}
	return facts, errs, nil
}

// Replay is the executor's replay claim, within factsTimeout; one that
// cannot be read is agenttool.ReplayUnknown.
func (r *Remote) Replay(ctx context.Context, c Call) agenttool.Replay {
	if !r.has(c.Tool) {
		return agenttool.ReplayUnknown
	}
	ctx, cancel := context.WithTimeout(ctx, factsTimeout)
	defer cancel()
	replay := agenttool.ReplayUnknown
	err := r.read(ctx, func(cn *conn) error {
		t, err := cn.tool(c.Tool)
		if err != nil {
			return err
		}
		if _, ok := t.(agenttool.Replayable); !ok {
			return nil
		}
		got, err := cn.c.Facts(ctx, mcpclient.FactsCall{Name: c.Tool, Args: c.Args})
		if err != nil {
			return err
		}
		replay = got[0].Replay
		return nil
	})
	if err != nil {
		return agenttool.ReplayUnknown
	}
	return replay
}

// Call runs the call in the executor. Its progress and its questions
// come back over the connection: the call's OnUpdate and the elicitor
// in ctx are the session's. A call is sent once: when the connection
// is lost while it is in flight, its error says so, and it is not sent
// again, since it may have run.
func (r *Remote) Call(ctx context.Context, c Call) (agenttool.Result, error) {
	if !r.has(c.Tool) {
		return agenttool.Result{}, fmt.Errorf("no tool named %q", c.Tool)
	}
	cn, err := r.conn(ctx)
	if err != nil {
		return agenttool.Result{}, err
	}
	t, err := cn.tool(c.Tool)
	if err != nil {
		return agenttool.Result{}, err
	}
	res, err := t.Execute(ctx, c.Call)
	// A tool's own error comes with its result's details; the
	// connection's comes with none.
	if err != nil && res.Details == nil && ctx.Err() == nil && r.lostUnder(cn, err) {
		return res, fmt.Errorf("executor: the connection was lost while the call was in flight, so it may or may not have run; it is not sent again, and the next call reconnects: %w", err)
	}
	return res, err
}

// FS is the executor's workspace's files, read-only, each request
// within filesTimeout (files.go), over the current connection
// (mcpworkspace.NewFS). It implements fs.StatFS, fs.ReadDirFS,
// fs.ReadFileFS and fs.ReadLinkFS; a name that leaves the workspace is
// workspace.ErrOutside, as the executor's own file system has it.
func (r *Remote) FS() fs.FS { return filesOf(r.readFiles) }

// readFiles is a request for the workspace's files on the connection,
// which has no effect: read again once on a new connection if the
// connection was lost under it.
func (r *Remote) readFiles(ctx context.Context, f func(*sdk.ClientSession) error) error {
	return r.read(ctx, func(cn *conn) error { return f(cn.c.Session()) })
}

// Descriptor is the workspace the executor's capability names.
func (r *Remote) Descriptor() workspace.Descriptor { return r.first.desc }

// Box is what the box host in front of the executor resolved for the
// session at the first connection, as JSON: {"profile", "units": [an
// envelope per unit]}; nil when no box host is.
func (r *Remote) Box() json.RawMessage { return r.first.box }

// Close ends the calls in flight and the connection, once, and stops
// a redial; nothing is dialed after it. The executor ends the
// processes it started for the session.
func (r *Remote) Close() error {
	r.once.Do(func() {
		r.mu.Lock()
		r.closed = true
		cn := r.cur
		r.cur = nil
		r.mu.Unlock()
		r.cancel()
		if cn != nil {
			r.closeErr = cn.close(errRemoteClosed)
		}
		r.bg.Wait()
	})
	return r.closeErr
}

// lostErr is err, a process request's on cn, with what it means when
// cn is lost (lostUnder).
func (cn *conn) lostErr(err error) error {
	switch cause := context.Cause(cn.procs); {
	case errors.Is(cause, errRemoteClosed):
	case errors.Is(cause, errProcessLost) || (cn.r != nil && cn.r.lostUnder(cn, err)):
		return fmt.Errorf("executor: %w: %w", errProcessLost, err)
	}
	return fmt.Errorf("executor: %w", err)
}
