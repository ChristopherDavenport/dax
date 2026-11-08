package agent

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/ChristopherDavenport/agenttool"
)

// MCPDeferBytes is the size of an MCP server's tool definitions, as a
// request carries them (name, description and parameters, JSON), past
// which a server whose config says nothing of defer has its tools held
// back until the model loads them. 8 KiB is about 2,000 tokens: a
// server or two of a few small tools stays in every request, where a
// tool_search call and the listing would cost about as much as they
// save, and the servers a person adds for reach rather than for every
// task (a web search's eight tools are 33 KiB, a browser's seventeen
// 26 KiB) are held back, which is most of what a request carried before
// the person's first prompt (dax#189).
const MCPDeferBytes = 8 << 10

// deferMCP is the session's agentkit.WithToolSearch function: an MCP
// server's tools are held back when its config's defer says so, and,
// when it says nothing, when their definitions pass MCPDeferBytes. It
// decides each server once, the first time the kit asks with the
// server's tools, and keeps the answer for the session, so a server
// whose list changes, or that restarts, neither moves its tools between
// the listing and the request nor breaks the cached prefix. A source
// that is no MCP server's is never held.
func (s *Session) deferMCP(source string, tools []agenttool.Tool) bool {
	if !strings.HasPrefix(source, "mcp:") || len(tools) == 0 {
		return false
	}
	s.serversMu.Lock()
	defer s.serversMu.Unlock()
	var info *MCPServerInfo
	for i := range s.servers {
		if strings.HasPrefix(tools[0].Name(), "mcp__"+s.servers[i].Name+"__") {
			info = &s.servers[i]
			break
		}
	}
	if info == nil {
		// Not a server the session knows (yet): offered as it was before
		// deferral, and asked again next time.
		return false
	}
	if d, ok := s.deferred[info.Name]; ok {
		return d
	}
	d := toolDefinitionBytes(tools) > MCPDeferBytes
	if info.deferSet != nil {
		d = *info.deferSet
	}
	if s.deferred == nil {
		s.deferred = map[string]bool{}
	}
	s.deferred[info.Name] = d
	return d
}

// deferDecided is deferMCP's answer for the server called name, false
// when it has not been asked.
func (s *Session) deferDecided(name string) bool {
	s.serversMu.Lock()
	defer s.serversMu.Unlock()
	return s.deferred[name]
}

// toolDefinitionBytes is the size of tools' definitions as a request
// carries them: each as a function tool's JSON, its name, description
// and parameters.
func toolDefinitionBytes(tools []agenttool.Tool) int {
	n := 0
	for _, t := range tools {
		data, err := json.Marshal(struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Parameters  json.RawMessage `json:"parameters,omitempty"`
		}{"function", t.Name(), t.Description(), t.Parameters()})
		if err == nil {
			n += len(data)
		}
	}
	return n
}

// loadedTools are the held-back tools the person's conversation has
// loaded (agentkit's ToolSearch.Loaded on the kit's own conversation),
// none without dax-toolsearch.
func (s *Session) loadedTools() []string {
	if s.Kit == nil {
		return nil
	}
	ts := s.Kit.ToolSearch()
	if ts == nil {
		return nil
	}
	return ts.Loaded(context.Background())
}
