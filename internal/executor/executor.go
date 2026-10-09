// Package executor is the session's execution plane for the
// extensions' tools: what runs a call, says what a call would touch,
// and says whether one may run again. The session decides on a call
// (the policy, the hooks, the record) and the executor carries it out,
// so the two may sit on different machines: Executor is the seam, and
// InProcess, the extensions' own tools in this process, is the case
// with the wire left out.
//
// Which tools are execution is fixed: every tool in an extension's
// Tools. Tools an extension adds through its Kit options (memory, the
// sub-agents, skill) are the session's control and never come here.
//
// Bind turns an executor's tools into agenttool tools for the kit, each
// an adapter whose every call goes through the executor, and keeps the
// facts each decision read (Set.Pin).
package executor

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/ChristopherDavenport/agenttool"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/extension"
)

// Executor runs the extensions' tools on behalf of a session, wherever
// they are. agenttool.Executor, the batch scheduler, is another thing.
type Executor interface {
	// Tools are the tools the executor runs, in the extensions' order.
	Tools(ctx context.Context) ([]Tool, error)
	// Facts is the tool's facts claim for the call (agenttool.Factual):
	// the zero Facts for a tool that makes none.
	Facts(ctx context.Context, c Call) (agenttool.Facts, error)
	// Replay is whether the call may run again (agenttool.ReplayOf).
	Replay(ctx context.Context, c Call) agenttool.Replay
	// Call runs the call. The elicitor and agentturn.Invoke travel in
	// ctx, as they do to any tool.
	Call(ctx context.Context, c Call) (agenttool.Result, error)
	// Descriptor is the workspace the tools act in.
	Descriptor() workspace.Descriptor
	// Close releases what the tools hold, once.
	Close() error
}

// Tool is one tool an executor runs, as the session sees it: what the
// model is offered and what the policy and the batch scheduler read.
type Tool struct {
	// Extension names the extension whose tool it is.
	Extension string
	// Definition is the function tool on a request, agenttool.Definition
	// of the tool.
	Definition  *openresponses.FunctionTool
	Annotations agenttool.Annotations
	// ReadOnly is the extension's ReadOnly naming the tool.
	ReadOnly   bool
	Sequential bool
	Resource   string
	// Factual is whether the tool makes the facts claim
	// (agenttool.IsFactual).
	Factual bool
	// Replayable is whether the tool answers whether a call may run
	// again (agenttool.Replayable); one that does not reads as
	// agenttool.ReplayUnknown.
	Replayable bool
}

// Name is the tool's name.
func (t Tool) Name() string { return t.Definition.Name }

// Call is one call of the tool named Tool.
type Call struct {
	Tool string
	agenttool.Call
}

// InProcess is the executor of exts' tools in this process: each
// extension's Tools, called once over env. A nil tool is refused, and
// what was built is closed when anything is.
func InProcess(exts []extension.Extension, env extension.ToolEnv) (Executor, error) {
	x, err := inProcessOf(exts, env)
	if err != nil {
		return nil, err
	}
	return x, nil
}

// inProcessOf is InProcess as its own type, which NewServer serves.
func inProcessOf(exts []extension.Extension, env extension.ToolEnv) (*inProcess, error) {
	x := &inProcess{ws: env.Workspace, byName: map[string]agenttool.Tool{}}
	for _, e := range exts {
		if e.Tools == nil {
			continue
		}
		for _, t := range e.Tools(env) {
			if t == nil {
				x.Close()
				return nil, fmt.Errorf("extension %s: a nil tool", e.Name)
			}
			x.inner = append(x.inner, t)
			_, replayable := t.(agenttool.Replayable)
			x.tools = append(x.tools, Tool{
				Extension:   e.Name,
				Definition:  agenttool.Definition(t),
				Annotations: agenttool.AnnotationsOf(t),
				ReadOnly:    slices.Contains(e.ReadOnly, t.Name()),
				Sequential:  agenttool.IsSequential(t),
				Resource:    agenttool.ResourceOf(t),
				Factual:     agenttool.IsFactual(t),
				Replayable:  replayable,
			})
			// Two tools of one name are the session's to refuse, with a
			// message that names both extensions; the first is the one
			// that runs until it does.
			if _, ok := x.byName[t.Name()]; !ok {
				x.byName[t.Name()] = t
			}
		}
	}
	return x, nil
}

type inProcess struct {
	ws     workspace.Workspace
	tools  []Tool
	inner  []agenttool.Tool
	byName map[string]agenttool.Tool

	once     sync.Once
	closeErr error
}

func (x *inProcess) Tools(context.Context) ([]Tool, error) { return slices.Clone(x.tools), nil }

func (x *inProcess) tool(name string) (agenttool.Tool, error) {
	t, ok := x.byName[name]
	if !ok {
		return nil, fmt.Errorf("no tool named %q", name)
	}
	return t, nil
}

func (x *inProcess) Facts(ctx context.Context, c Call) (agenttool.Facts, error) {
	t, err := x.tool(c.Tool)
	if err != nil {
		return agenttool.Facts{}, err
	}
	f, _, err := agenttool.FactsOf(ctx, t, c.Args)
	return f, err
}

func (x *inProcess) Replay(ctx context.Context, c Call) agenttool.Replay {
	t, err := x.tool(c.Tool)
	if err != nil {
		return agenttool.ReplayUnknown
	}
	return agenttool.ReplayOf(ctx, t, c.Args)
}

func (x *inProcess) Call(ctx context.Context, c Call) (agenttool.Result, error) {
	t, err := x.tool(c.Tool)
	if err != nil {
		return agenttool.Result{}, err
	}
	return t.Execute(ctx, c.Call)
}

func (x *inProcess) Descriptor() workspace.Descriptor {
	if x.ws == nil {
		return workspace.Descriptor{}
	}
	return x.ws.Descriptor()
}

// Close closes the tools that hold something, once.
func (x *inProcess) Close() error {
	x.once.Do(func() { x.closeErr = agenttool.Set(x.inner).Close() })
	return x.closeErr
}
