package agent

// An MCP server's prompts as slash commands: /mcp__<server>__<prompt>
// args sends what the server's prompts/get answers, marked as composed
// from that prompt and untrusted, a third party's text sent with the
// person's authority (agenttool RFC 0001's MCP prompts; agentsession's
// instructions_part and trust).

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPPrompt is a prompt one of the session's MCP servers offers, as
// the command that runs it.
type MCPPrompt struct {
	// Command is mcp__<server>__<prompt>, the slash command's name.
	Command     string
	Description string
	// Arguments are the prompt's declared arguments, in order.
	Arguments []string
	prompt    string
	server    string
}

// mcpPromptSource is the instructions layer source of an MCP prompt's
// messages on the record.
const mcpPromptSource = "dax.mcp_prompt"

// MCPPrompts lists the prompts the session's MCP servers offer now,
// asked of each server (prompts/list), so a server's change is seen at
// the next listing. A server that offers none, or cannot answer, adds
// none.
func (s *Session) MCPPrompts(ctx context.Context) []MCPPrompt {
	var out []MCPPrompt
	for _, srv := range s.Kit.MCPServers() {
		sess := srv.Remote.Session()
		if sess == nil || sess.InitializeResult() == nil || sess.InitializeResult().Capabilities == nil || sess.InitializeResult().Capabilities.Prompts == nil {
			continue
		}
		for p, err := range sess.Prompts(ctx, nil) {
			if err != nil {
				break
			}
			var args []string
			for _, a := range p.Arguments {
				args = append(args, a.Name)
			}
			out = append(out, MCPPrompt{Command: srv.Remote.Name(p.Name), Description: p.Description, Arguments: args, prompt: p.Name, server: srv.Label})
		}
	}
	slices.SortFunc(out, func(a, b MCPPrompt) int { return strings.Compare(a.Command, b.Command) })
	return out
}

// MCPPrompt is the messages /<command> args sends: the server's
// prompts/get with args split on white space into the declared
// arguments in order, the last taking the rest. Each text message is
// marked with the prompt as its instructions layer and as untrusted;
// content that is not text is left out.
func (s *Session) MCPPrompt(ctx context.Context, command, args string) (openresponses.Items, error) {
	i := slices.IndexFunc(s.MCPPrompts(ctx), func(p MCPPrompt) bool { return p.Command == command })
	if i < 0 {
		return nil, fmt.Errorf("no MCP prompt /%s", command)
	}
	p := s.MCPPrompts(ctx)[i]
	remote, ok := s.Kit.MCP(p.server)
	if !ok {
		return nil, fmt.Errorf("/%s: its server is gone", command)
	}
	given := map[string]string{}
	rest := strings.TrimSpace(args)
	for j, name := range p.Arguments {
		if rest == "" {
			break
		}
		if j == len(p.Arguments)-1 {
			given[name] = rest
			break
		}
		word, after, _ := strings.Cut(rest, " ")
		given[name], rest = word, strings.TrimSpace(after)
	}
	res, err := remote.Session().GetPrompt(ctx, &mcp.GetPromptParams{Name: p.prompt, Arguments: given})
	if err != nil {
		return nil, fmt.Errorf("/%s: %w", command, err)
	}
	prov := agentturn.Provenance{
		Layers: []agentturn.Layer{{ID: command, Source: mcpPromptSource}},
		Trust:  &agentturn.Trust{Reads: []agenttool.Label{agenttool.LabelUntrusted}},
	}
	var out openresponses.Items
	for _, m := range res.Messages {
		t, ok := m.Content.(*mcp.TextContent)
		if !ok || t.Text == "" {
			continue
		}
		var item openresponses.Item = openresponses.UserText(t.Text)
		if m.Role == "assistant" {
			item = openresponses.AssistantText(t.Text)
		}
		marked, err := agentturn.WithProvenance(item, prov)
		if err != nil {
			return nil, err
		}
		out = append(out, marked)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("/%s: the server's prompt has no text to send", command)
	}
	return out, nil
}
