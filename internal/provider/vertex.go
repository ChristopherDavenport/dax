package provider

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/ChristopherDavenport/dax/internal/modelinfo"
	"github.com/ChristopherDavenport/openresponses"
)

// Vertex AI serves more than one vendor's models behind one project,
// location and login, each family through its own API. The vertex
// provider is one streamer that sends each request to the adapter for
// the family its model names, so the main agent and the sub-agents can
// run different families and /model can move between them.
const (
	familyClaude = "claude"
	familyGemini = "gemini"
)

// vertexFamily is the family a Vertex model ID names, empty for one dax
// cannot reach. IDs are Vertex's own (claude-opus-5-5, gemini-2.5-pro),
// not resource paths.
func vertexFamily(model string) string {
	switch {
	case strings.HasPrefix(model, "claude-"):
		return familyClaude
	case strings.HasPrefix(model, "gemini-"):
		return familyGemini
	}
	return ""
}

func errVertexModel(model string) error {
	return fmt.Errorf("vertex: model %q is not a claude- or gemini- model", model)
}

// vertexModels routes a request by its model to the Claude or the
// Gemini adapter. Neither compacts, so it does not either.
type vertexModels struct {
	openresponses.UnsupportedCompaction

	claude, gemini openresponses.Streamer
}

func (v vertexModels) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	switch vertexFamily(req.Model) {
	case familyClaude:
		return v.claude.CreateStream(ctx, req, sink)
	case familyGemini:
		return v.gemini.CreateStream(ctx, req, sink)
	}
	return errVertexModel(req.Model)
}

// vertexDescriber says what a Vertex model takes. Vertex publishes no
// capabilities for either family (its models.get carries a name and a
// version, nothing more), so what is known is read off the model ID's
// generation, as the gemini adapter reads its thinking encoding. The
// rules are what Vertex accepted from each model when they were written;
// a model they do not cover is unknown, and its effort goes out as
// configured.
type vertexDescriber struct{}

var (
	claudeOff    = efforts("none")
	claude46     = efforts("none", "minimal", "low", "medium", "high")
	claude55     = efforts("minimal", "low", "medium", "high", "xhigh")
	geminiLevels = efforts("low", "medium", "high")
)

func efforts(names ...string) []openresponses.ReasoningEffort {
	out := make([]openresponses.ReasoningEffort, len(names))
	for i, n := range names {
		out[i] = openresponses.ReasoningEffort(n)
	}
	return out
}

func (vertexDescriber) Describe(_ context.Context, model string) (modelinfo.Info, error) {
	info := modelinfo.Info{Source: "vertex", Reasons: modelinfo.Yes}
	switch vertexFamily(model) {
	case familyClaude:
		major, minor, ok := claudeGeneration(model)
		switch v := major*100 + minor; {
		case !ok:
			return modelinfo.Info{}, nil
		case v < 406:
			// The anthropic adapter sends an effort as adaptive
			// thinking, which Claude takes from 4.6 on.
			info.Efforts = claudeOff
		case v == 406:
			info.Efforts = claude46
		case v >= 505:
			// Claude 5.5 turns thinking off with a thinking type the
			// adapter cannot send yet, and refuses the one it does.
			info.Efforts = claude55
		default:
			return modelinfo.Info{}, nil
		}
	case familyGemini:
		// Every Gemini model Vertex serves takes these: minimal is
		// refused by some Gemini 3 models, none by Gemini 3 and by
		// 2.5 Pro, and the adapter has no xhigh.
		info.Efforts = geminiLevels
	default:
		return modelinfo.Info{}, errVertexModel(model)
	}
	return info, nil
}

// claudeGeneration reads the version a Claude model ID names, in either
// order Anthropic has used: claude-opus-4-5 and claude-sonnet-5, or the
// older claude-3-7-sonnet. A dated suffix is not a version.
func claudeGeneration(model string) (major, minor int, ok bool) {
	var nums []int
	for _, part := range strings.Split(strings.TrimPrefix(model, "claude-"), "-") {
		n, err := strconv.Atoi(part)
		if err != nil || len(part) > 2 {
			continue
		}
		if nums = append(nums, n); len(nums) == 2 {
			break
		}
	}
	switch len(nums) {
	case 0:
		return 0, 0, false
	case 1:
		return nums[0], 0, true
	}
	return nums[0], nums[1], true
}
