package agent

// What a front over the Agent Client Protocol (dax's acp.go) asks of the
// session beyond the control contract: the editor's MCP servers, and
// the conversation as the record holds it, to replay on session/load.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/internal/config"
)

// AddEditorMCP starts an MCP server the editor names for this session
// (ACP's mcpServers): a command with its arguments, placed on this
// machine whatever the workspace, its env given to that process alone,
// as a secret_env server's values are, and redacted from its output,
// since what an editor puts there may be a token. A name the user's
// config, the workspace or a server already started gives is refused.
// The editor's servers are this session's: Reopen does not start them
// again. It returns the label RemoveMCP takes.
func (s *Session) AddEditorMCP(ctx context.Context, name, program string, args []string, env map[string]string) (string, error) {
	if program == "" {
		return "", fmt.Errorf("mcp %s: no command", name)
	}
	for _, u := range s.reserved {
		if collidesWithOffered(name, u) {
			return "", fmt.Errorf("mcp %s: the workspace's server %s offers tools under that name", name, u)
		}
	}
	for _, m := range s.mcpNames {
		if config.MCPNamesCollide(name, m) {
			return "", fmt.Errorf("mcp %s: your config's server %s offers tools under that name; an editor's server may not stand in for one of yours", name, m)
		}
	}
	s.serversMu.Lock()
	for _, a := range s.added {
		if config.MCPNamesCollide(name, a.server.Name) {
			s.serversMu.Unlock()
			return "", fmt.Errorf("mcp %s: a server started in this session offers tools under that name", name)
		}
	}
	s.serversMu.Unlock()
	words := append([]string{program}, args...)
	for i, w := range words {
		words[i] = shellQuote(w)
	}
	m := MCPServer{Name: name, Command: strings.Join(words, " "), Host: true, Description: "the editor's"}
	if len(env) > 0 {
		m.secretValues = env
		for k := range env {
			m.SecretEnv = append(m.SecretEnv, k)
		}
		slices.Sort(m.SecretEnv)
	}
	t, mopts, info, err := s.mcpStart(ctx, m)
	if err != nil {
		return "", err
	}
	label, err := s.Kit.AddMCPTransport(ctx, t, mopts...)
	if err != nil {
		return "", err
	}
	info.Added = label
	s.serversMu.Lock()
	s.servers = append(s.servers, info)
	s.serversMu.Unlock()
	return label, nil
}

// shellQuote is w as one word a shell, and cmdline.Split, reads back as
// w.
func shellQuote(w string) string {
	if w != "" && !strings.ContainsAny(w, " \t\n'\"\\$`|&;<>()*?[]{}~#!") {
		return w
	}
	return "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
}

// Recorded is one item of the conversation as the record holds it.
type Recorded struct {
	Item openresponses.Item
	// Failed marks a function call output the record says failed.
	Failed bool
}

// Conversation is the conversation on the session's path, from the
// record: the items in order, each output marked failed when the record
// says so, for a front that replays it (ACP's session/load).
func (s *Session) Conversation(ctx context.Context) ([]Recorded, error) {
	if !s.recorded {
		return nil, errors.New("no session store: nothing is recorded")
	}
	sess, err := s.store.Open(ctx, s.ID())
	if err != nil {
		return nil, err
	}
	var out []Recorded
	for _, e := range sess.Path(sess.Leaf()) {
		if it, ok := e.(*agentsession.ItemEntry); ok && it.Item != nil {
			out = append(out, Recorded{Item: it.Item, Failed: it.Failed})
		}
	}
	return out, nil
}
