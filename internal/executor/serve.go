package executor

import (
	"errors"
	"fmt"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agenttool/mcpserver"
	workspace "github.com/ChristopherDavenport/agentworkspace"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ChristopherDavenport/dax/extension"
)

// CapabilityKey is the experimental capability under which an executor
// served by NewServer says what a session needs of it that MCP has no
// field for: the workspace's descriptor, the extensions whose tools it
// runs, and each tool's extension, read-only, facts and strict, in the
// order the tools run in process; and the resource template its
// workspace's files are read under (files.go). The go-sdk lists tools
// sorted by name, and dax's per-extension ReadOnly is not the
// annotation hint, so neither can be read off the listing.
//
//	{"version": 1,
//	 "descriptor": {"kind": "container", "ref": "box", "root": "/work"},
//	 "extensions": ["dax-coding"],
//	 "tools": [{"name": "read", "extension": "dax-coding", "readOnly": true, "facts": true}, ...],
//	 "files": {"uriTemplate": "dax-workspace:///{op}{?path}"}}
//
// files came after version 1 was released, as a field a client of
// version 1 ignores; a client that reads it refuses an executor
// without it.
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
}

// capabilityFiles says the workspace's files are served, read-only, as
// MCP resources under URITemplate (FilesURITemplate).
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
}

// NewServer serves exts' tools, built once over env as InProcess builds
// them, as an MCP server: `dax execute`, run where the tools are to
// act. The tools served are the tools themselves, not adapters, so
// their facts claims, their replay claims, the stamp a decision's facts
// put on a call and the check of it when the call runs are all made
// here, in this process, under this process's stamp key. The server
// answers mcpserver's facts method and advertises its capability, and
// says under CapabilityKey what MCP cannot carry. It says the tool list
// never changes, so a client does not wait for a notification after
// every call.
//
// It also serves env's workspace's files, read-only, as resources
// under FilesURITemplate, so that the session reads the project's
// AGENTS.md, .dax/skills and .dax/config.json where the project is.
//
// closeTools closes the tools; the caller closes env's workspace after it.
// Two tools of one name are refused: the session refuses them anyway,
// and a capability that named one twice could not say which ran.
func NewServer(o ServeOptions, exts []extension.Extension, env extension.ToolEnv) (srv *sdk.Server, closeTools func() error, err error) {
	x, err := inProcessOf(exts, env)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil {
			x.Close()
		}
	}()
	d := o.Descriptor
	if d == (workspace.Descriptor{}) {
		d = x.Descriptor()
	}
	if env.Workspace == nil {
		return nil, nil, errors.New("executor: no workspace")
	}
	c := capability{
		Version: CapabilityVersion, Descriptor: capabilityDescriptor{Kind: d.Kind, Ref: d.Ref, Root: d.Root},
		Extensions: []string{}, Tools: []capabilityTool{},
		Files: &capabilityFiles{URITemplate: FilesURITemplate},
	}
	for _, e := range exts {
		if e.Tools != nil {
			c.Extensions = append(c.Extensions, e.Name)
		}
	}
	seen := map[string]string{}
	for i, t := range x.tools {
		if other, ok := seen[t.Name()]; ok {
			return nil, nil, fmt.Errorf("executor: two tools named %q, of %s and %s", t.Name(), other, t.Extension)
		}
		seen[t.Name()] = t.Extension
		c.Tools = append(c.Tools, capabilityTool{
			Name: t.Name(), Extension: t.Extension, ReadOnly: t.ReadOnly,
			Facts: t.Factual, Strict: agenttool.IsStrict(x.inner[i]),
		})
	}
	srv = sdk.NewServer(&sdk.Implementation{Name: o.Name, Version: o.Version}, &sdk.ServerOptions{
		Capabilities: &sdk.ServerCapabilities{
			Experimental: map[string]any{CapabilityKey: c},
			Tools:        &sdk.ToolCapabilities{ListChanged: false},
			Resources:    &sdk.ResourceCapabilities{ListChanged: false},
		},
	})
	serveFiles(srv, env.Workspace.FS())
	if err := mcpserver.AddTools(srv, x.inner...); err != nil {
		return nil, nil, err
	}
	return srv, x.Close, nil
}
