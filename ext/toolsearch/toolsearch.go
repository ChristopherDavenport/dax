// Package toolsearch is dax-toolsearch, the extension that holds the
// session's large MCP servers' tools back until the model asks for them
// (dax#189): agentkit's WithToolSearch over the session's choice of
// servers (extension.Env's DeferMCP, from mcp_servers' defer, else by
// size), which lists a held tool by name and summary in the
// instructions and offers tool_search to load it. A loaded tool is sent
// from the next request on, after the tools already sent, and stays for
// the conversation, so the cached prefix holds; the record has it as a
// config entry's tools_added, and a resumed session keeps it loaded. It
// is built only from what package extension offers any extension.
package toolsearch

import (
	"github.com/ChristopherDavenport/agentkit"

	"github.com/ChristopherDavenport/dax/extension"
	"github.com/ChristopherDavenport/dax/policy"
)

// Name is the extension's name, and so its rules' source:
// extension:dax-toolsearch.
const Name = "dax-toolsearch"

// New is dax-toolsearch. tool_search is allowed: it reads the session's
// own tool list and changes what the model is offered, never what it
// may do, since a loaded tool's call is decided by its server's rules
// as before. Without the extension (dax.WithoutExtension(Name)) every
// server's tools are sent in every request, as before dax v0.0.35.
func New() extension.Extension {
	return extension.Extension{
		Name:   Name,
		Owns:   []string{agentkit.ToolSearchTool},
		Policy: policy.Rules{Allow: []string{agentkit.ToolSearchTool}},
		Kit: func(env extension.Env) ([]agentkit.Option, error) {
			return []agentkit.Option{agentkit.WithToolSearch(env.DeferMCP)}, nil
		},
	}
}
