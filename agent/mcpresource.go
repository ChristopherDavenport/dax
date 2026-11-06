package agent

// An MCP server's resources as @ mentions: @<server>:<uri> in a typed
// line attaches what the server's resources/read answers, marked as
// composed from that resource and untrusted, a third party's text sent
// with the person's authority (agenttool RFC 0001's MCP resources;
// agentsession's instructions_part and trust), beside the line, which
// stays the person's words (#176).

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPResource is a resource one of the session's MCP servers lists, as
// the mention that attaches it.
type MCPResource struct {
	// Mention is @<server>:<uri>.
	Mention     string
	Name        string
	Description string
}

// mcpResourceSource is the instructions layer source of an attached
// resource on the record.
const mcpResourceSource = "dax.mcp_resource"

// maxResource bounds what one mention attaches of a resource's text,
// and maxMentioned what one line's mentions attach in all.
const (
	maxResource  = 100 << 10
	maxMentioned = 256 << 10
)

// mention is @<server>:<uri> at the start of a line or after a space;
// what ends a sentence is not part of the URI.
var mention = regexp.MustCompile(`(?:^|\s)@([A-Za-z0-9_-]+):(\S+)`)

// mcpByName are the session's MCP servers that offer resources, by the
// name their tools carry (mcp__<name>__<tool>).
func (s *Session) mcpByName() map[string]string {
	out := map[string]string{}
	for _, srv := range s.Kit.MCPServers() {
		sess := srv.Remote.Session()
		if sess == nil || sess.InitializeResult() == nil || sess.InitializeResult().Capabilities == nil || sess.InitializeResult().Capabilities.Resources == nil {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(srv.Remote.Name(""), "mcp__"), "__")
		out[name] = srv.Label
	}
	return out
}

// IsMCPMention reports whether word is a mention of a resource of one
// of the session's MCP servers (@<server>:<uri>, the server's name
// one the session has): such a word is the server's, never a path, even
// when its URI ends as an image's name does.
func (s *Session) IsMCPMention(word string) bool {
	m := mention.FindStringSubmatch(word)
	if m == nil || !strings.HasPrefix(word, "@") {
		return false
	}
	_, ok := s.mcpByName()[m[1]]
	return ok
}

// MCPResources lists the resources the session's MCP servers offer now
// (resources/list), as mentions, sorted. A server that offers none, or
// cannot answer, adds none; a URI with space in it cannot be typed as a
// mention and is left out.
func (s *Session) MCPResources(ctx context.Context) []MCPResource {
	var out []MCPResource
	for name, label := range s.mcpByName() {
		remote, ok := s.Kit.MCP(label)
		if !ok || remote.Session() == nil {
			continue
		}
		for r, err := range remote.Session().Resources(ctx, nil) {
			if err != nil {
				break
			}
			if r.URI == "" || strings.ContainsFunc(r.URI, func(c rune) bool { return c <= ' ' }) {
				continue
			}
			out = append(out, MCPResource{Mention: "@" + name + ":" + r.URI, Name: r.Name, Description: r.Description})
		}
	}
	slices.SortFunc(out, func(a, b MCPResource) int { return strings.Compare(a.Mention, b.Mention) })
	return out
}

// MCPMentions are the items the resources text mentions attach: for
// each @<server>:<uri> naming one of the session's servers, once
// however often it is mentioned, the server's resources/read as one
// user message marked with the resource as its instructions layer and
// as untrusted. A mention of a name no server has is text, and adds
// nothing. A resource's text is cut at 100 KiB; content that is not
// text is said to be left out. What the line's mentions attach in all is
// cut at 256 KiB.
func (s *Session) MCPMentions(ctx context.Context, text string) (openresponses.Items, error) {
	found := mention.FindAllStringSubmatch(text, -1)
	if len(found) == 0 {
		return nil, nil
	}
	servers := s.mcpByName()
	var out openresponses.Items
	seen := map[string]bool{}
	left := maxMentioned
	for _, m := range found {
		name, uri := m[1], strings.TrimRight(m[2], ".,;!?")
		label, ok := servers[name]
		key := name + ":" + uri
		if !ok || uri == "" || seen[key] {
			continue
		}
		seen[key] = true
		remote, ok := s.Kit.MCP(label)
		if !ok || remote.Session() == nil {
			return nil, fmt.Errorf("@%s: its server is gone", key)
		}
		res, err := remote.Session().ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
		if err != nil {
			return nil, fmt.Errorf("@%s: %w", key, err)
		}
		item, err := agentturn.WithProvenance(openresponses.UserText(resourceText(name, uri, res.Contents, &left)), agentturn.Provenance{
			Layers: []agentturn.Layer{{ID: key, Source: mcpResourceSource}},
			Trust:  &agentturn.Trust{Reads: []agenttool.Label{agenttool.LabelUntrusted}},
		})
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

// resourceText is a resource's contents as the message that attaches
// it: each text whole, or cut on a rune boundary at maxResource or at
// what *left says the line may still attach, which it lowers; content
// that is not text is said to be left out.
func resourceText(name, uri string, contents []*mcp.ResourceContents, left *int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<mcp_resource server=%q uri=%q>\n", name, uri)
	for _, c := range contents {
		switch {
		case c.Text != "":
			t, note := c.Text, ""
			if n := min(maxResource, *left); len(t) > n {
				for n > 0 && !utf8.RuneStart(t[n]) {
					n-- // on a rune's first byte
				}
				t = t[:n]
				note = fmt.Sprintf("\n[cut: %d of %d bytes attached]", len(t), len(c.Text))
				if n < maxResource {
					note = fmt.Sprintf("\n[cut: %d of %d bytes attached; a line attaches at most %d bytes]", len(t), len(c.Text), maxMentioned)
				}
			}
			*left -= len(t)
			t += note
			b.WriteString(t)
			if !strings.HasSuffix(t, "\n") {
				b.WriteString("\n")
			}
		case len(c.Blob) > 0:
			fmt.Fprintf(&b, "[%s: %d bytes of %s, not attached]\n", c.URI, len(c.Blob), c.MIMEType)
		}
	}
	b.WriteString("</mcp_resource>")
	return b.String()
}
