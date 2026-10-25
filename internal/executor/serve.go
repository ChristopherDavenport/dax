package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agenttool/mcpserver"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/agentworkspace/mcpworkspace"
	"github.com/ChristopherDavenport/agentworkspace/mcpworkspace/wire"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/tool"
)

// CapabilityKey is the experimental capability under which an executor
// served by NewServer says what a session needs of it that MCP has no
// field for: the workspace's descriptor, the extensions whose tools it
// runs, and each tool's extension, read-only, facts and strict, in the
// order the tools run in process; and the resource template its
// workspace's files are read under (files.go, mcpworkspace's). The go-sdk lists tools
// sorted by name, and dax's per-extension ReadOnly is not the
// annotation hint, so neither can be read off the listing.
//
//	{"version": 1,
//	 "descriptor": {"kind": "container", "ref": "box", "root": "/work"},
//	 "extensions": ["dax-coding"],
//	 "tools": [{"name": "read", "extension": "dax-coding", "readOnly": true, "facts": true}, ...],
//	 "files": {"uriTemplate": "agentworkspace:///{op}{?path}"},
//	 "maxReadBytes": 2097152}
//
// files came after version 1 was released, as a field a client of
// version 1 ignores; a client that reads it refuses an executor
// without it. start came after files, the same way: it says the
// executor answers the process methods (process.go, mcpworkspace's
// agentworkspace/process.*), with their bounds (wire.Bounds), and is absent when its workspace cannot start a process; a
// client refuses to start a process in an executor without it, and
// starts none anywhere else.
//
//	"start": {"maxProcesses": 32, "maxWriteBytes": 1048576, "maxReadBytes": 65536}
//
// maxReadBytes came after start, the same way: the most bytes of a
// file this session's tools read (read scans, edit rewrites, the bash
// analyzer reads), which the session asked for and the executor capped
// (ServeOptions.MaxReadBytes). An executor older than it reports none,
// and applies its own -max-read-bytes whatever the session asked.
//
// A client says what it asks of the executor under the same key in its
// own capabilities, at initialize, so that it is the same over stdio
// and over HTTP:
//
//	{"maxReadBytes": 1048576}
//
// where 0 or no field asks for tool.DefaultMaxRead. A client older than
// that says nothing, and its session's tools read what the executor's
// ceiling says, as before.
const CapabilityKey = "io.github.christopherdavenport.dax/executor"

// CapabilityVersion is the version of CapabilityKey's value. A client
// refuses one it does not know.
const CapabilityVersion = 1

// capability is CapabilityKey's value.
type capability struct {
	Version    int                  `json:"version"`
	Descriptor capabilityDescriptor `json:"descriptor"`
	// Extensions are the extensions with Tools, in order, whether or
	// not they built any.
	Extensions []string `json:"extensions"`
	// Tools are the tools in the order they run in process.
	Tools []capabilityTool `json:"tools"`
	// Files names how the workspace's files are read; nil from an
	// executor older than the field.
	Files *capabilityFiles `json:"files,omitempty"`
	// Start says the executor starts processes for the session (the
	// process methods); nil from an executor older than the field or
	// whose workspace cannot.
	Start *wire.Bounds `json:"start,omitempty"`
	// Servers says the executor's workspace offers MCP servers of its
	// own (workspace.ServerProvider), which the server methods list and
	// start; false from an executor older than the field. A box host in
	// front of the executor offers its units whatever this says.
	Servers bool `json:"servers,omitempty"`
	// MaxReadBytes is the most bytes of a file the session's tools
	// read; zero from an executor older than the field, whose tools
	// read what its own -max-read-bytes says.
	MaxReadBytes int64 `json:"maxReadBytes,omitempty"`
	// Box is what a box host in front of the executor resolved for the
	// session (`dax box serve` adds it to the handshake's answer): the
	// profile and each unit's envelope. dax execute itself never sets it.
	Box json.RawMessage `json:"box,omitempty"`
}

// clientCapability is CapabilityKey's value in a client's capabilities:
// what the session asks of the executor.
type clientCapability struct {
	// MaxReadBytes is the session's max_read_bytes; zero asks for
	// tool.DefaultMaxRead. The executor caps it (readLimit).
	MaxReadBytes int64 `json:"maxReadBytes,omitempty"`
	// Box is the box the session asks a box host for (`dax box serve`
	// reads it from the first message and passes it on unchanged); the
	// executor in the box reads nothing of it.
	Box json.RawMessage `json:"box,omitempty"`
}

// capabilityFiles says the workspace's files are served, read-only, as
// MCP resources under URITemplate (wire.FilesURITemplate).
type capabilityFiles struct {
	URITemplate string `json:"uriTemplate"`
}

// capabilityDescriptor is a workspace.Descriptor on the wire.
type capabilityDescriptor struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref,omitempty"`
	Root string `json:"root"`
}

// capabilityTool is what the capability says of one tool.
type capabilityTool struct {
	Name      string `json:"name"`
	Extension string `json:"extension"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
	Facts     bool   `json:"facts,omitempty"`
	Strict    bool   `json:"strict,omitempty"`
}

// ServeOptions name the server an executor is served as and the
// workspace it reports.
type ServeOptions struct {
	// Name and Version are the server's implementation, the program's.
	Name, Version string
	// Descriptor is the workspace the tools act in as the session
	// records it. The zero value is the workspace's own.
	Descriptor workspace.Descriptor
	// Done, once closed, ends every process the server started for
	// its sessions and every request waiting on one, and refuses
	// another start: close it before a session's graceful close
	// (Server.Run's, when its context ends), which waits for the
	// requests in flight. nil is never.
	Done <-chan struct{}
	// MaxReadBytes is the ceiling on a session's read limit, set by
	// whoever runs the executor (`dax execute -max-read-bytes`): a
	// session's tools read what it asks, up to this, and a client that
	// asks nothing (an older dax) gets this. Zero is tool.DefaultMaxRead,
	// so that reading more than the default is the executor's choice.
	MaxReadBytes int64
}

// Extensions builds the extensions whose tools a session is served,
// for a session whose read limit is maxRead (dax-coding's coding.New
// takes it): `dax execute`'s are the program's extensions as its
// settings choose them. It is called for every session, and once more
// when the server is made, to check what it builds.
type Extensions func(maxRead int64) ([]extension.Extension, error)

// Server is `dax execute`'s MCP server, NewServer's: each session it
// serves has an MCP server of its own, whose tools are built for that
// session when it initializes, at the read limit it asked for, and
// closed when it ends. Every session shares the workspace, the
// tool.Files over it (and so the lock its writes take), the files
// served and the process methods.
type Server struct {
	o       ServeOptions
	exts    Extensions
	env     extension.ToolEnv // the session's MaxReadBytes is set on a copy
	desc    capabilityDescriptor
	procs   *mcpworkspace.Processes // nil when the workspace cannot start a process
	ceiling int64                   // o.MaxReadBytes, or tool.DefaultMaxRead

	mu       sync.Mutex
	middle   []sdk.Middleware
	sessions map[*sdk.ServerSession]*sessionTools
	closed   bool
	// closing counts the tools of ended sessions being closed, which
	// Close waits for.
	closing sync.WaitGroup
}

// sessionTools are one session's tools.
type sessionTools struct {
	x       *inProcess
	maxRead int64
}

// NewServer serves exts' tools, built for each session over env as
// InProcess builds them, as an MCP server: `dax execute`, run where
// the tools are to act. The tools served are the tools themselves, not
// adapters, so their facts claims, their replay claims, the stamp a
// decision's facts put on a call and the check of it when the call
// runs are all made here, in this process, under this process's stamp
// key. Each session's server answers mcpserver's facts method and
// advertises its capability, and says under CapabilityKey what MCP
// cannot carry. It says the tool list never changes, so a client does
// not wait for a notification after every call.
//
// A session's tools are built when it initializes, from exts at the
// session's read limit, with env's MaxReadBytes set to it: what the
// client asked under CapabilityKey, capped at o.MaxReadBytes, or that
// ceiling when it asked nothing. They are closed when the session ends.
//
// It also serves env's workspace's files, read-only, as resources
// under wire.FilesURITemplate (mcpworkspace.ServeFiles), so that the session reads the project's
// AGENTS.md, .dax/skills and .dax/config.json where the project is;
// and, when the workspace can start a process (workspace.Starter),
// the process methods (mcpworkspace.Processes), so that the MCP servers the
// session's user configured run here too. Those are custom methods,
// not tools: no tool of the server starts a process the session did
// not ask for, and no other client sees one in the listing.
//
// The caller closes the Server, and env's workspace after it. Two
// tools of one name are refused: the session refuses them anyway, and
// a capability that named one twice could not say which ran. NewServer
// builds the tools once at the ceiling, and closes them, to refuse that
// before any session comes.
func NewServer(o ServeOptions, exts Extensions, env extension.ToolEnv) (*Server, error) {
	if env.Workspace == nil {
		return nil, errors.New("executor: no workspace")
	}
	if o.MaxReadBytes < 0 {
		return nil, fmt.Errorf("executor: a read limit of %d bytes", o.MaxReadBytes)
	}
	s := &Server{o: o, exts: exts, env: env, ceiling: o.MaxReadBytes, sessions: map[*sdk.ServerSession]*sessionTools{}}
	if s.ceiling == 0 {
		s.ceiling = tool.DefaultMaxRead
	}
	d := o.Descriptor
	if d == (workspace.Descriptor{}) {
		d = env.Workspace.Descriptor()
	}
	s.desc = capabilityDescriptor{Kind: d.Kind, Ref: d.Ref, Root: d.Root}
	x, _, err := s.build(s.ceiling)
	if err != nil {
		return nil, err
	}
	if err := x.Close(); err != nil {
		return nil, err
	}
	if _, ok := env.Workspace.(workspace.Starter); ok {
		s.procs = mcpworkspace.NewProcesses(env.Workspace)
		if o.Done != nil {
			go func(procs *mcpworkspace.Processes) {
				<-o.Done
				procs.Close()
			}(s.procs)
		}
	}
	return s, nil
}

// readLimit is the read limit of a session that asked for asked (ok:
// it asked; an older client does not), under ceiling: what it asked,
// zero being tool.DefaultMaxRead, capped at the ceiling; or the ceiling
// itself when it asked nothing, which is what the executor's flag gave
// every session before sessions asked.
func readLimit(ceiling, asked int64, ok bool) int64 {
	if !ok {
		return ceiling
	}
	if asked <= 0 {
		asked = tool.DefaultMaxRead
	}
	return min(asked, ceiling)
}

// build builds the tools at the read limit maxRead, and the capability
// that says them.
func (s *Server) build(maxRead int64) (*inProcess, capability, error) {
	exts, err := s.exts(maxRead)
	if err != nil {
		return nil, capability{}, err
	}
	env := s.env
	env.MaxReadBytes = maxRead
	x, err := inProcessOf(exts, env)
	if err != nil {
		return nil, capability{}, err
	}
	c := capability{
		Version: CapabilityVersion, Descriptor: s.desc,
		Extensions: []string{}, Tools: []capabilityTool{},
		Files:        &capabilityFiles{URITemplate: wire.FilesURITemplate},
		MaxReadBytes: maxRead,
	}
	for _, e := range exts {
		if e.Tools != nil {
			c.Extensions = append(c.Extensions, e.Name)
		}
	}
	seen := map[string]string{}
	for i, t := range x.tools {
		if other, ok := seen[t.Name()]; ok {
			x.Close()
			return nil, capability{}, fmt.Errorf("executor: two tools named %q, of %s and %s", t.Name(), other, t.Extension)
		}
		seen[t.Name()] = t.Extension
		c.Tools = append(c.Tools, capabilityTool{
			Name: t.Name(), Extension: t.Extension, ReadOnly: t.ReadOnly,
			Facts: t.Factual, Strict: agenttool.IsStrict(x.inner[i]),
		})
	}
	if _, ok := env.Workspace.(workspace.Starter); ok {
		b := wire.DefaultBounds()
		c.Start = &b
		_, c.Servers = env.Workspace.(workspace.ServerProvider)
	}
	return x, c, nil
}

// AddReceivingMiddleware wraps the requests of every session connected
// after it, once the session has initialized (its initialize itself is
// not wrapped), outside everything the server answers: a program
// watching or holding the requests, as the tests do.
func (s *Server) AddReceivingMiddleware(mw ...sdk.Middleware) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.middle = append(s.middle, mw...)
}

// Connect serves one session over t, on a server of its own, as
// sdk.Server's Connect does.
func (s *Server) Connect(ctx context.Context, t sdk.Transport, opts *sdk.ServerSessionOptions) (*sdk.ServerSession, error) {
	srv, err := s.session()
	if err != nil {
		return nil, err
	}
	return srv.Connect(ctx, t, opts)
}

// Run serves one session over t until the client hangs up or ctx ends,
// as sdk.Server's Run does: `dax execute` on standard input and
// output.
func (s *Server) Run(ctx context.Context, t sdk.Transport) error {
	srv, err := s.session()
	if err != nil {
		return err
	}
	return srv.Run(ctx, t)
}

// Sessions are the sessions that have initialized and not ended.
func (s *Server) Sessions() []*sdk.ServerSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Collect(maps.Keys(s.sessions))
}

// endSession ends ss's processes and the requests waiting on them
// (mcpworkspace.Processes.End).
func (s *Server) endSession(ss *sdk.ServerSession) {
	if s.procs != nil {
		s.procs.End(ss)
	}
}

// Close closes the tools of every session that has not ended, waits
// for those of the sessions that have, and refuses another session;
// the sessions themselves are their transports' to end. The caller
// closes the workspace after it.
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	open := slices.Collect(maps.Values(s.sessions))
	clear(s.sessions)
	s.mu.Unlock()
	var errs []error
	for _, st := range open {
		errs = append(errs, st.x.Close())
	}
	s.closing.Wait()
	return errors.Join(errs...)
}

var errServerClosed = errors.New("executor: the server is closed")

// session is a new MCP server for one session: the files and the
// process methods, the facts method answering for no tool, and the
// tools added when the session initializes (built). One MCP server
// serves one session: a second refuses.
func (s *Server) session() (*sdk.Server, error) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, errServerClosed
	}
	srv := sdk.NewServer(&sdk.Implementation{Name: s.o.Name, Version: s.o.Version}, &sdk.ServerOptions{
		Capabilities: &sdk.ServerCapabilities{
			Tools:     &sdk.ToolCapabilities{ListChanged: false},
			Resources: &sdk.ResourceCapabilities{ListChanged: false},
		},
	})
	mcpworkspace.ServeFiles(srv, s.env.Workspace.FS())
	if s.procs != nil {
		if err := s.procs.Register(srv); err != nil {
			return nil, err
		}
	}
	// The facts method and its capability are the server's from the
	// start, answering for no tool until the session's are added; the
	// request that adds them, its initialize, has its handlers already.
	if err := mcpserver.AddTools(srv); err != nil {
		return nil, err
	}
	b := &builder{s: s, srv: srv}
	srv.AddReceivingMiddleware(b.middleware)
	return srv, nil
}

// builder builds one session's tools, on its server, when the session
// starts: at its initialize, or, from a client of the protocol without
// one, at its discover, once that has started the session (over the
// stateful HTTP transport, a discover is a probe the SDK answers and
// closes, and the client initializes after it).
type builder struct {
	s   *Server
	srv *sdk.Server

	mu   sync.Mutex
	ss   *sdk.ServerSession // the session built for
	c    capability
	err  error
	done bool
}

func (b *builder) middleware(next sdk.MethodHandler) sdk.MethodHandler {
	return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
		ss, ok := req.GetSession().(*sdk.ServerSession)
		if !ok {
			return next(ctx, method, req)
		}
		_, discover := req.(*sdk.ServerRequest[*sdk.DiscoverParams])
		_, initialize := req.(*sdk.ServerRequest[*sdk.InitializeParams])
		var c capability
		if !discover && (initialize || ss.InitializeParams() != nil) {
			// Built before the initialize is answered, so that a
			// session whose tools cannot be built does not start.
			var err error
			if c, err = b.ensure(ss, req); err != nil {
				return nil, err
			}
		}
		res, err := next(ctx, method, req)
		if err != nil {
			return res, err
		}
		switch r := res.(type) {
		case *sdk.InitializeResult:
			r.Capabilities = withCapability(r.Capabilities, c)
		case *sdk.DiscoverResult:
			if ss.InitializeParams() == nil {
				// The probe: no session, so no tools.
				break
			}
			if c, err = b.ensure(ss, req); err != nil {
				return nil, err
			}
			r.Capabilities = withCapability(r.Capabilities, c)
		}
		return res, nil
	}
}

// withCapability is caps with c under CapabilityKey, on a copy of its
// experimental map.
func withCapability(caps *sdk.ServerCapabilities, c capability) *sdk.ServerCapabilities {
	if caps == nil {
		caps = &sdk.ServerCapabilities{}
	}
	exp := maps.Clone(caps.Experimental)
	if exp == nil {
		exp = map[string]any{}
	}
	exp[CapabilityKey] = c
	caps.Experimental = exp
	return caps
}

// ensure builds ss's tools, once, at the read limit req's client asked
// for, and adds them to the server; a session's build that failed
// fails each of its requests.
func (b *builder) ensure(ss *sdk.ServerSession, req sdk.Request) (capability, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.done {
		if b.ss != ss {
			return capability{}, errors.New("executor: this server serves one session")
		}
		return b.c, b.err
	}
	b.done, b.ss = true, ss
	b.c, b.err = b.build(ss, req)
	return b.c, b.err
}

func (b *builder) build(ss *sdk.ServerSession, req sdk.Request) (capability, error) {
	s := b.s
	asked, ok, err := askedOf(clientCapabilities(ss, req))
	if err != nil {
		return capability{}, err
	}
	x, c, err := s.build(readLimit(s.ceiling, asked, ok))
	if err != nil {
		return capability{}, err
	}
	if err := mcpserver.AddTools(b.srv, x.inner...); err != nil {
		x.Close()
		return capability{}, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		x.Close()
		return capability{}, errServerClosed
	}
	s.sessions[ss] = &sessionTools{x: x, maxRead: c.MaxReadBytes}
	b.srv.AddReceivingMiddleware(s.middle...)
	s.mu.Unlock()
	go func() {
		ss.Wait()
		s.mu.Lock()
		st, ok := s.sessions[ss]
		delete(s.sessions, ss)
		if ok {
			s.closing.Add(1)
		}
		s.mu.Unlock()
		if ok {
			st.x.Close()
			s.closing.Done()
		}
	}()
	return c, nil
}

// clientCapabilities are the capabilities req's client gave: in an
// initialize's parameters, or, from a client of the protocol without
// one, on the request; nil when it gave none.
func clientCapabilities(ss *sdk.ServerSession, req sdk.Request) *sdk.ClientCapabilities {
	switch r := req.(type) {
	case *sdk.ServerRequest[*sdk.InitializeParams]:
		if r.Params != nil {
			return r.Params.Capabilities
		}
		return nil
	case *sdk.ServerRequest[*sdk.DiscoverParams]:
		return r.ClientCapabilities()
	}
	if p := ss.InitializeParams(); p != nil {
		return p.Capabilities
	}
	return nil
}

// askedOf is the read limit caps ask for under CapabilityKey; ok is
// false when they say nothing under it, as an older client's do. A
// value that is not the capability is refused.
func askedOf(caps *sdk.ClientCapabilities) (asked int64, ok bool, err error) {
	if caps == nil {
		return 0, false, nil
	}
	v, ok := caps.Experimental[CapabilityKey]
	if !ok {
		return 0, false, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return 0, false, fmt.Errorf("executor: the client's %s: %w", CapabilityKey, err)
	}
	var cc clientCapability
	if err := json.Unmarshal(raw, &cc); err != nil {
		return 0, false, fmt.Errorf("executor: the client's %s: %w", CapabilityKey, err)
	}
	return cc.MaxReadBytes, true, nil
}
