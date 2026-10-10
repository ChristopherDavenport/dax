package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agenttool/mcpclient"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// factsTimeout bounds one reading of a call's facts claim from an
// executor elsewhere. A reading that takes longer fails, and a failed
// reading blocks the call. A var for the tests.
var factsTimeout = 30 * time.Second

// ConnectOptions name the session to the executor.
type ConnectOptions struct {
	// Name and Version are the client's implementation, the program's.
	Name, Version string
}

// Remote is the executor NewServer serves, reached over MCP: `dax
// execute` in a container or on another host, its tools' facts and
// replay claims read there, their calls run there, and their stamps
// made and checked there.
type Remote struct {
	c          *mcpclient.Remote
	server     sdk.Implementation
	desc       workspace.Descriptor
	extensions []string
	tools      []Tool
	byName     map[string]agenttool.Tool

	once     sync.Once
	closeErr error
}

var _ Executor = (*Remote)(nil)

// Connect connects to the executor at the other end of t and checks
// that it is one: that it answers the facts method, and that its
// capability (CapabilityKey) is of a version this client reads and
// names every tool it lists, no more, each claiming facts exactly when
// the listed tool does. A server that fails any of these is refused,
// since a session that took its tools anyway would decide their calls
// on what the model wrote, not on what they would touch, which is a
// wider policy than the user's. The caller closes the Remote.
func Connect(ctx context.Context, t sdk.Transport, o ConnectOptions) (*Remote, error) {
	c, err := mcpclient.Connect(ctx, t,
		mcpclient.WithClaims(),
		mcpclient.WithElicitation(),
		mcpclient.WithClientInfo(o.Name, o.Version),
		// The executor says its tool list never changes; a call does
		// not wait for a notification after it.
		mcpclient.WithNotificationGrace(0),
	)
	if err != nil {
		return nil, fmt.Errorf("executor: %w", err)
	}
	r, err := remoteOf(c)
	if err != nil {
		c.Close()
		return nil, err
	}
	return r, nil
}

// errNotExecutor is the start of every refusal of a server that does
// not say what a session needs of an executor.
var errNotExecutor = errors.New("not a dax executor")

// remoteOf checks what c's server said at initialize and listed, and
// builds the tools in the capability's order.
func remoteOf(c *mcpclient.Remote) (*Remote, error) {
	server := c.ServerInfo()
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("executor (%s %s): %w: %s", server.Name, server.Version, errNotExecutor, fmt.Sprintf(format, args...))
	}
	init := c.Session().InitializeResult()
	if init == nil || init.Capabilities == nil {
		return nil, refuse("it gave no capabilities")
	}
	exp := init.Capabilities.Experimental
	if _, ok := exp[mcpclient.FactsCapability]; !ok {
		return nil, refuse("it does not answer %s (no %s capability)", mcpclient.FactsMethod, mcpclient.FactsCapability)
	}
	v, ok := exp[CapabilityKey]
	if !ok {
		return nil, refuse("no %s capability", CapabilityKey)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, refuse("%s: %v", CapabilityKey, err)
	}
	var cp capability
	if err := json.Unmarshal(raw, &cp); err != nil {
		return nil, refuse("%s: %v", CapabilityKey, err)
	}
	if cp.Version != CapabilityVersion {
		return nil, refuse("%s version %d; this client reads version %d", CapabilityKey, cp.Version, CapabilityVersion)
	}
	if cp.Files == nil {
		return nil, refuse("it does not serve its workspace's files (no files in %s), which the session reads AGENTS.md, .dax/skills and .dax/config.json through; update dax execute where it runs", CapabilityKey)
	}
	if cp.Files.URITemplate != FilesURITemplate {
		return nil, refuse("it serves its workspace's files under %q; this client reads %q", cp.Files.URITemplate, FilesURITemplate)
	}
	if cp.Descriptor.Kind == "" || cp.Descriptor.Root == "" {
		return nil, refuse("its capability names no workspace (kind %q, root %q)", cp.Descriptor.Kind, cp.Descriptor.Root)
	}
	listed := map[string]agenttool.Tool{}
	for _, t := range c.Tools() {
		listed[t.Name()] = t
	}
	r := &Remote{
		c: c, server: server,
		desc:       workspace.Descriptor{Kind: cp.Descriptor.Kind, Ref: cp.Descriptor.Ref, Root: cp.Descriptor.Root},
		extensions: slices.Clone(cp.Extensions),
		byName:     map[string]agenttool.Tool{},
	}
	for _, ct := range cp.Tools {
		t, ok := listed[ct.Name]
		switch {
		case ct.Name == "":
			return nil, refuse("its capability names a tool with no name")
		case r.byName[ct.Name] != nil:
			return nil, refuse("its capability names %q twice", ct.Name)
		case !ok:
			return nil, refuse("its capability names %q, which it does not list", ct.Name)
		case !slices.Contains(cp.Extensions, ct.Extension):
			return nil, refuse("its capability says %q is of extension %q, which it does not name", ct.Name, ct.Extension)
		case ct.Facts && !agenttool.IsFactual(t):
			return nil, refuse("its capability says %q claims its facts, but the tool is listed without the claim", ct.Name)
		case !ct.Facts && agenttool.IsFactual(t):
			return nil, refuse("%q is listed with a facts claim its capability does not name", ct.Name)
		}
		tl := toolOf(ct.Extension, t, ct.ReadOnly)
		if ct.Strict {
			strict := true
			tl.Definition.Strict = &strict
		}
		r.tools = append(r.tools, tl)
		r.byName[ct.Name] = t
	}
	for name := range listed {
		if r.byName[name] == nil {
			return nil, refuse("it lists %q, which its capability does not name", name)
		}
	}
	return r, nil
}

// Server is the name and version the executor gave at initialize.
func (r *Remote) Server() (name, version string) { return r.server.Name, r.server.Version }

// Extensions are the extensions whose tools the executor runs, in its
// order, whether or not they built any.
func (r *Remote) Extensions() []string { return slices.Clone(r.extensions) }

func (r *Remote) Tools(context.Context) ([]Tool, error) { return slices.Clone(r.tools), nil }

func (r *Remote) tool(name string) (agenttool.Tool, error) {
	t, ok := r.byName[name]
	if !ok {
		return nil, fmt.Errorf("no tool named %q", name)
	}
	return t, nil
}

// Facts reads the call's claim in the executor, within factsTimeout.
// An error, the executor's, the connection's or the timeout's, is
// returned, and blocks the call.
func (r *Remote) Facts(ctx context.Context, c Call) (agenttool.Facts, error) {
	t, err := r.tool(c.Tool)
	if err != nil {
		return agenttool.Facts{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, factsTimeout)
	defer cancel()
	f, _, err := agenttool.FactsOf(ctx, t, c.Args)
	if err != nil {
		return agenttool.Facts{}, fmt.Errorf("executor: %w", err)
	}
	return f, nil
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
	got, err := r.c.Facts(ctx, ask...)
	if err != nil {
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
	t, err := r.tool(c.Tool)
	if err != nil {
		return agenttool.ReplayUnknown
	}
	ctx, cancel := context.WithTimeout(ctx, factsTimeout)
	defer cancel()
	return agenttool.ReplayOf(ctx, t, c.Args)
}

// Call runs the call in the executor. Its progress and its questions
// come back over the connection: the call's OnUpdate and the elicitor
// in ctx are the session's.
func (r *Remote) Call(ctx context.Context, c Call) (agenttool.Result, error) {
	t, err := r.tool(c.Tool)
	if err != nil {
		return agenttool.Result{}, err
	}
	return t.Execute(ctx, c.Call)
}

// FS is the executor's workspace's files, read-only, each request
// within filesTimeout (files.go). It implements fs.StatFS,
// fs.ReadDirFS, fs.ReadFileFS and fs.ReadLinkFS; a name that leaves the
// workspace is workspace.ErrOutside, as the executor's own file system
// has it.
func (r *Remote) FS() fs.FS { return remoteFS{r.c.Session()} }

// Descriptor is the workspace the executor's capability names.
func (r *Remote) Descriptor() workspace.Descriptor { return r.desc }

// Close ends the calls in flight and the connection, once.
func (r *Remote) Close() error {
	r.once.Do(func() { r.closeErr = r.c.Close() })
	return r.closeErr
}
