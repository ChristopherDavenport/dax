package agent

// What one tool result sends the model is capped: what it returns stays
// in every later request until a fold. A result over its cap is sent as
// its first and last halves, with how much was left out between them,
// and the whole is kept as a typed blob of the session, named in the
// note as sidecar:sha256:<hex> (which keeps it while the session is
// kept), so the model reads it back with read_output and it never lands
// in the user's repository or a remote workspace (#172).

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/dax/internal/config"
)

// ownTools are dax's tools that bound what they return themselves, and
// so are not cut here: the file tools, the background tool, read_output
// itself, skill and memory's.
var ownTools = []string{"read", "write", "edit", "glob", "grep", "ls", "background", "read_output", "skill"}

// outputMediaType is a kept output's type.
const outputMediaType = "text/plain"

// keptOutputs are the blobs the session kept a tool's whole output in.
type keptOutputs struct {
	mu   sync.Mutex
	kept map[string]bool
}

// capOf is the most bytes a result of the tool called name sends the
// model: bash_max_output for bash, mcp_max_output for a tool that is
// not dax's own (an MCP server's, a sub-agent's, a peer's, another
// extension's), none for dax's own.
func (s *Session) capOf(name string) int {
	switch {
	case name == "bash":
		if s.opts.BashMaxOutput > 0 {
			return s.opts.BashMaxOutput
		}
		return config.DefaultBashMaxOutput
	case slices.Contains(ownTools, name) || strings.HasPrefix(name, "memory_"):
		return 0
	}
	if s.opts.ToolMaxOutput > 0 {
		return s.opts.ToolMaxOutput
	}
	return config.DefaultToolMaxOutput
}

// capTool is t with its results cut to its cap (capOf); t itself for a
// tool with none.
func (s *Session) capTool(t agenttool.Tool) agenttool.Tool {
	limit := s.capOf(t.Name())
	if t == nil || limit <= 0 {
		return t
	}
	return agenttool.Wrap(t, func(ctx context.Context, c agenttool.Call) (agenttool.Result, error) {
		res, err := t.Execute(ctx, c)
		if err != nil {
			return res, err
		}
		res.Output.Text = s.cut(ctx, res.Output.Text, limit)
		for i, p := range res.Output.Parts {
			if tp, ok := p.(*openresponses.InputText); ok && len(tp.Text) > limit {
				cp := *tp
				cp.Text = s.cut(ctx, tp.Text, limit)
				res.Output.Parts[i] = &cp
			}
		}
		return res, nil
	})
}

// cut is text as the model is sent it under limit: whole when it fits,
// else its first and last halves with a note between them naming the
// blob the whole is kept in.
func (s *Session) cut(ctx context.Context, text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	head, tail := runeCut(text[:limit/2], true), runeCut(text[len(text)-limit/2:], false)
	left := len(text) - len(head) - len(tail)
	note := fmt.Sprintf("\n... [%d of %d bytes left out here", left, len(text))
	if hash, err := s.keepOutput(ctx, text); err == nil {
		note += fmt.Sprintf("; the whole output is kept with the session as sidecar:%s, which read_output reads (by lines, or the lines matching a pattern)", hash)
	} else {
		note += "; the whole output is not kept (" + err.Error() + ")"
	}
	return head + note + "] ...\n" + tail
}

// runeCut trims s to whole runes: a head loses a partial last rune, a
// tail a partial first one.
func runeCut(s string, head bool) string {
	if head {
		for len(s) > 0 && !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
		return s
	}
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

// errNoBlobs is a store that keeps no blobs.
var errNoBlobs = errors.New("this session's store keeps no blobs")

// keepOutput stores text as a typed blob of the session's store and
// returns its hash.
func (s *Session) keepOutput(ctx context.Context, text string) (string, error) {
	tw, ok := s.store.(agentsession.TypedBlobWriter)
	if !ok {
		return "", errNoBlobs
	}
	hash, err := tw.PutTypedBlob(context.WithoutCancel(ctx), []byte(text), outputMediaType)
	if err != nil {
		return "", err
	}
	s.outs.mu.Lock()
	if s.outs.kept == nil {
		s.outs.kept = map[string]bool{}
	}
	s.outs.kept[hash] = true
	s.outs.mu.Unlock()
	return hash, nil
}

// ToolOutput is the whole of a tool output the session cut and kept,
// by its reference (sidecar:sha256:<hex>, or the hash alone): only one
// this session kept, or its record names, never another blob of the
// store.
func (s *Session) ToolOutput(ctx context.Context, ref string) ([]byte, error) {
	hash := strings.TrimPrefix(strings.TrimSpace(ref), "sidecar:")
	if !agentsession.ValidHash(hash) {
		return nil, fmt.Errorf("%q is not an output's reference (sidecar:sha256:<hex>)", ref)
	}
	br, ok := s.store.(agentsession.BlobReader)
	if !ok {
		return nil, errNoBlobs
	}
	s.outs.mu.Lock()
	kept := s.outs.kept[hash]
	s.outs.mu.Unlock()
	if !kept && !s.recordNames(ctx, hash) {
		return nil, fmt.Errorf("no output %s was kept by this session", ref)
	}
	data, _, err := agentsession.ReadSessionBlob(ctx, br, s.ID(), hash)
	return data, err
}

// recordNames says whether the session's record names the blob: an
// output kept before a restart.
func (s *Session) recordNames(ctx context.Context, hash string) bool {
	r, ok := s.store.(agentsession.Reader)
	if !ok {
		return false
	}
	sess, err := r.Read(ctx, s.ID())
	if err != nil {
		return false
	}
	names, err := agentsession.SessionBlobs(sess)
	return err == nil && slices.Contains(names, hash)
}
