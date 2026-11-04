package modelinfo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"google.golang.org/genai"

	"github.com/ChristopherDavenport/openresponses"
)

const none = openresponses.ReasoningEffortNone

// noReasoning is a model that takes no effort but none.
var noReasoning = []openresponses.ReasoningEffort{none}

// Anthropic describes a model from GET /v1/models/{id}. The efforts are
// those the anthropic adapter can send: it maps an effort onto
// output_config.effort, so a model without effort support takes only
// none (thinking disabled), whatever thinking it supports otherwise.
func Anthropic(models *sdk.ModelService) Describer { return anthropicDescriber{models} }

type anthropicDescriber struct{ models *sdk.ModelService }

func (d anthropicDescriber) Describe(ctx context.Context, model string) (Info, error) {
	m, err := d.models.Get(ctx, model, sdk.ModelGetParams{})
	if err != nil {
		return Info{}, err
	}
	c := m.Capabilities
	info := Info{Source: "anthropic", Reasons: No, Efforts: noReasoning, ContextWindow: m.MaxInputTokens, MaxOutput: m.MaxTokens}
	if c.Thinking.Supported {
		info.Reasons = Yes
	}
	if c.Effort.Supported {
		efforts := []openresponses.ReasoningEffort{none}
		for _, l := range []struct {
			e  openresponses.ReasoningEffort
			ok bool
		}{
			{openresponses.ReasoningEffortLow, c.Effort.Low.Supported},
			{openresponses.ReasoningEffortMedium, c.Effort.Medium.Supported},
			{openresponses.ReasoningEffortHigh, c.Effort.High.Supported},
			{openresponses.ReasoningEffortXHigh, c.Effort.Xhigh.Supported},
			// The adapter cannot send max yet; it is listed so the
			// banner shows the model's range.
			{"max", c.Effort.Max.Supported},
		} {
			if l.ok {
				efforts = append(efforts, l.e)
			}
		}
		info.Efforts = efforts
	}
	return info, nil
}

// Gemini describes a model from models.get. Gemini says whether a model
// thinks but not which levels it takes, nor whether thinking can be
// turned off, so the efforts of a thinking model are unknown.
func Gemini(models *genai.Models) Describer { return geminiDescriber{models} }

type geminiDescriber struct{ models *genai.Models }

func (d geminiDescriber) Describe(ctx context.Context, model string) (Info, error) {
	m, err := d.models.Get(ctx, model, nil)
	if err != nil {
		return Info{}, err
	}
	info := Info{Source: "gemini", Reasons: No, Efforts: noReasoning, ContextWindow: int64(m.InputTokenLimit), MaxOutput: int64(m.OutputTokenLimit)}
	if m.Thinking {
		info.Reasons, info.Efforts = Yes, nil
	}
	return info, nil
}

// OpenRouter describes a model from the catalogue at {base}/models,
// which carries each model's reasoning: whether it is mandatory, the
// efforts it takes and its default. There is no endpoint for one
// model, so the catalogue (some hundreds of kilobytes) is fetched once
// and kept. No key is sent; the catalogue is public.
func OpenRouter(base string, client *http.Client) Describer {
	return &openRouterDescriber{url: strings.TrimSuffix(base, "/") + "/models", client: client}
}

type openRouterDescriber struct {
	url    string
	client *http.Client

	mu     sync.Mutex
	models map[string]openRouterModel
}

type openRouterModel struct {
	ID                  string   `json:"id"`
	ContextLength       int64    `json:"context_length"`
	SupportedParameters []string `json:"supported_parameters"`
	TopProvider         struct {
		MaxCompletionTokens int64 `json:"max_completion_tokens"`
	} `json:"top_provider"`
	Reasoning *struct {
		Mandatory        bool                            `json:"mandatory"`
		SupportedEfforts []openresponses.ReasoningEffort `json:"supported_efforts"`
		DefaultEffort    openresponses.ReasoningEffort   `json:"default_effort"`
	} `json:"reasoning"`
}

func (d *openRouterDescriber) Describe(ctx context.Context, model string) (Info, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.models == nil {
		var body struct {
			Data []openRouterModel `json:"data"`
		}
		if err := getJSON(ctx, d.client, http.MethodGet, d.url, nil, &body); err != nil {
			return Info{}, err
		}
		d.models = make(map[string]openRouterModel, len(body.Data))
		for _, m := range body.Data {
			d.models[m.ID] = m
		}
	}
	m, ok := d.models[model]
	if !ok {
		return Info{}, fmt.Errorf("openrouter has no model %q", model)
	}
	info := Info{Source: "openrouter", ContextWindow: m.ContextLength, MaxOutput: m.TopProvider.MaxCompletionTokens}
	switch {
	case m.Reasoning != nil:
		info.Reasons = Yes
		efforts := slices.Clone(m.Reasoning.SupportedEfforts)
		if !m.Reasoning.Mandatory {
			efforts = append(efforts, none)
		}
		info.Efforts = sortEfforts(efforts)
		info.DefaultEffort = m.Reasoning.DefaultEffort
	case slices.Contains(m.SupportedParameters, "reasoning"):
		info.Reasons = Yes
	default:
		info.Reasons, info.Efforts = No, noReasoning
	}
	return info, nil
}

// Ollama describes a model from POST {root}/api/show and GET
// {root}/api/ps, where root is the server the /v1 endpoint is under.
// Ollama refuses any effort but none for a model without the thinking
// capability; which levels a thinking model takes depends on the
// model, so those are unknown.
//
// The window is the one the server runs the model at, not the length
// it was trained to, which /api/show's model_info gives: past the
// served window Ollama drops the oldest messages and answers as if
// nothing happened, so a budget sized from the trained length never
// folds. A loaded model's context_length in /api/ps is the window, or
// else a num_ctx the model's parameters set; the trained length is an
// upper bound on either, and the window only when the server says
// nothing else (a model not loaded yet, with no num_ctx).
func Ollama(base string, client *http.Client) Describer {
	root := strings.TrimSuffix(strings.TrimSuffix(base, "/"), "/v1")
	return ollamaDescriber{url: root + "/api/show", ps: root + "/api/ps", client: client}
}

type ollamaDescriber struct {
	url, ps string
	client  *http.Client
}

func (d ollamaDescriber) Describe(ctx context.Context, model string) (Info, error) {
	var show struct {
		Capabilities []string                   `json:"capabilities"`
		Parameters   string                     `json:"parameters"`
		ModelInfo    map[string]json.RawMessage `json:"model_info"`
	}
	req, _ := json.Marshal(map[string]string{"model": model})
	if err := getJSON(ctx, d.client, http.MethodPost, d.url, req, &show); err != nil {
		return Info{}, err
	}
	info := Info{Source: "ollama", Reasons: No, Efforts: noReasoning}
	if slices.Contains(show.Capabilities, "thinking") {
		info.Reasons, info.Efforts = Yes, nil
	}
	var trained int64
	for k, v := range show.ModelInfo {
		if strings.HasSuffix(k, ".context_length") {
			_ = json.Unmarshal(v, &trained)
		}
	}
	served, from := d.served(ctx, model), "ollama /api/ps"
	if served == 0 {
		served, from = numCtx(show.Parameters), "ollama num_ctx"
	}
	switch {
	case served > 0 && (trained == 0 || served < trained):
		info.ContextWindow, info.WindowFrom = served, from
	case trained > 0:
		info.ContextWindow, info.WindowFrom = trained, "ollama trained length"
	}
	return info, nil
}

// served is the context length the server runs model at while it is
// loaded (/api/ps), zero when it is not loaded or the server does not
// say.
func (d ollamaDescriber) served(ctx context.Context, model string) int64 {
	var ps struct {
		Models []struct {
			Name          string `json:"name"`
			Model         string `json:"model"`
			ContextLength int64  `json:"context_length"`
		} `json:"models"`
	}
	if getJSON(ctx, d.client, http.MethodGet, d.ps, nil, &ps) != nil {
		return 0
	}
	for _, m := range ps.Models {
		if m.Name == model || m.Model == model {
			return m.ContextLength
		}
	}
	return 0
}

// numCtx is the num_ctx a model's parameters set, zero for none. The
// parameters are lines of a name and a value, as a Modelfile's.
func numCtx(params string) int64 {
	for line := range strings.SplitSeq(params, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "num_ctx" {
			if n, err := strconv.ParseInt(f[1], 10, 64); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

// getJSON makes one request and decodes a 200 response into out. An
// error carries the status and the start of the body, never a header.
func getJSON(ctx context.Context, client *http.Client, method, url string, body []byte, out any) error {
	if client == nil {
		client = http.DefaultClient
	}
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 200))
		return fmt.Errorf("%s %s: %s: %s", method, url, res.Status, strings.TrimSpace(string(msg)))
	}
	return json.NewDecoder(res.Body).Decode(out)
}
