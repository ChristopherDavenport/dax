package agent

// What one tool result sends the model is capped: what it returns stays
// in every later request until a fold. A result over its cap is sent as
// its first and last halves, with how much was left out between them,
// and the whole is kept as a typed blob of the session's store, never
// in the user's repository or a remote workspace (#172). The result
// names it by an opaque output id, never by the blob's hash: a
// dax.output_kept entry of the session maps the id to the blob, its
// sidecar: reference a member of its own, which keeps the blob while
// the session is kept and which a redaction pass reads as structure,
// not as text it might leave. read_output takes only an id this
// session kept.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/ChristopherDavenport/agentkit"
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

// keptOutputs are the blobs the session kept a tool's whole output in,
// by output id.
type keptOutputs struct {
	mu   sync.Mutex
	kept map[string]string
}

// outputKeptNS is the namespace of the entry that maps an output id to
// its blob.
const outputKeptNS = "dax.output_kept"

// outputKept is that entry's data: Blob is the sidecar: reference whole.
type outputKept struct {
	ID   string `json:"id"`
	Blob string `json:"blob"`
}

// outputID is an output's id: out_ and 16 random hex digits, which
// names no blob and no other session's output.
var outputID = regexp.MustCompile(`^out_[0-9a-f]{16}$`)

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
	if id, err := s.keepOutput(ctx, text); err == nil {
		note += fmt.Sprintf("; the whole output is kept with the session as output %s, which read_output reads (by lines, or the lines matching a pattern)", id)
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

// keepOutput stores text as a typed blob of the session's store,
// records the id it is kept under in the run's session (the call's
// own, a sub-agent's among them), and returns the id.
func (s *Session) keepOutput(ctx context.Context, text string) (string, error) {
	tw, ok := s.store.(agentsession.TypedBlobWriter)
	if !ok {
		return "", errNoBlobs
	}
	ctx = context.WithoutCancel(ctx)
	hash, err := tw.PutTypedBlob(ctx, []byte(text), outputMediaType)
	if err != nil {
		return "", err
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	id := "out_" + hex.EncodeToString(b[:])
	rec := agentkit.RecorderFromContext(ctx)
	if rec == nil {
		rec = s.Kit.Recorder()
	}
	if rec == nil {
		return "", errors.New("this session is not recorded")
	}
	if _, err := rec.Annotate(ctx, outputKeptNS, outputKept{ID: id, Blob: "sidecar:" + hash}); err != nil {
		return "", err
	}
	s.outs.mu.Lock()
	if s.outs.kept == nil {
		s.outs.kept = map[string]string{}
	}
	s.outs.kept[id] = hash
	s.outs.mu.Unlock()
	return id, nil
}

// ToolOutput is the whole of a tool output the session cut and kept,
// by the output id the cut result names: only an id this session kept,
// in this process or, after a restart, by its record's dax.output_kept
// entries; never a blob by its hash, nor another session's output.
func (s *Session) ToolOutput(ctx context.Context, id string) ([]byte, error) {
	id = strings.TrimSpace(id)
	if !outputID.MatchString(id) {
		return nil, fmt.Errorf("%q is not an output's id (out_ and 16 hex digits, as a cut result names it)", id)
	}
	br, ok := s.store.(agentsession.BlobReader)
	if !ok {
		return nil, errNoBlobs
	}
	s.outs.mu.Lock()
	hash, kept := s.outs.kept[id]
	s.outs.mu.Unlock()
	if !kept {
		hash, kept = s.recordKept(ctx, id)
	}
	if !kept {
		return nil, fmt.Errorf("no output %s was kept by this session", id)
	}
	data, _, err := agentsession.ReadSessionBlob(ctx, br, s.ID(), hash)
	return data, err
}

// recordKept is the blob the session's record keeps output id in, by
// its dax.output_kept entries: an output kept before a restart.
func (s *Session) recordKept(ctx context.Context, id string) (string, bool) {
	r, ok := s.store.(agentsession.Reader)
	if !ok {
		return "", false
	}
	sess, err := r.Read(ctx, s.ID())
	if err != nil {
		return "", false
	}
	for _, e := range sess.Entries() {
		c, ok := e.(*agentsession.CustomEntry)
		if !ok || c.NS != outputKeptNS {
			continue
		}
		var k outputKept
		if json.Unmarshal(c.Data, &k) == nil && k.ID == id {
			hash, ok := strings.CutPrefix(k.Blob, "sidecar:")
			return hash, ok && agentsession.ValidHash(hash)
		}
	}
	return "", false
}
