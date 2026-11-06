package modelinfo

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"google.golang.org/genai"
)

func serve(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

const openRouterCatalogue = `{"data":[
 {"id":"anthropic/claude-sonnet-5.5","context_length":1000000,"top_provider":{"max_completion_tokens":128000},
  "supported_parameters":["reasoning","tools"],
  "reasoning":{"mandatory":true,"supported_efforts":["max","xhigh","high","medium","low"],"default_effort":"high"}},
 {"id":"openai/gpt-5-mini","context_length":400000,"supported_parameters":["reasoning"],
  "reasoning":{"mandatory":false,"supported_efforts":["high","low","turbo"]}},
 {"id":"vendor/thinker","supported_parameters":["reasoning","include_reasoning"]},
 {"id":"vendor/plain","context_length":8192,"supported_parameters":["tools"]}
]}`

func TestOpenRouter(t *testing.T) {
	var fetches atomic.Int32
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models" || r.Method != http.MethodGet {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("a key was sent to the public catalogue")
		}
		fetches.Add(1)
		io.WriteString(w, openRouterCatalogue)
	})
	d := OpenRouter(srv.URL+"/api/v1/", nil)
	ctx := context.Background()
	tests := []struct {
		model   string
		reasons Support
		efforts []E
		def     E
		ctxWin  int64
		out     int64
	}{
		{"anthropic/claude-sonnet-5.5", Yes, []E{"low", "medium", "high", "xhigh", "max"}, "high", 1_000_000, 128_000},
		// Not mandatory: none joins, in order; an effort Order does not
		// know is left out.
		{"openai/gpt-5-mini", Yes, []E{"none", "low", "high"}, "", 400_000, 0},
		{"vendor/thinker", Yes, nil, "", 0, 0},
		{"vendor/plain", No, []E{"none"}, "", 8192, 0},
	}
	for _, tc := range tests {
		info, err := d.Describe(ctx, tc.model)
		if err != nil {
			t.Fatalf("%s: %v", tc.model, err)
		}
		if info.Source != "openrouter" || info.Reasons != tc.reasons || !slices.Equal(info.Efforts, tc.efforts) ||
			info.DefaultEffort != tc.def || info.ContextWindow != tc.ctxWin || info.MaxOutput != tc.out {
			t.Errorf("%s: %+v", tc.model, info)
		}
	}
	if _, err := d.Describe(ctx, "no/such"); err == nil || !strings.Contains(err.Error(), `no model "no/such"`) {
		t.Errorf("unknown model: %v", err)
	}
	if n := fetches.Load(); n != 1 {
		t.Errorf("the catalogue was fetched %d times, want once", n)
	}
}

func TestOpenRouterFailureIsNotKept(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "upstream down", http.StatusBadGateway)
			return
		}
		io.WriteString(w, openRouterCatalogue)
	})
	d := OpenRouter(srv.URL, nil)
	if _, err := d.Describe(context.Background(), "vendor/plain"); err == nil || !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), "upstream down") {
		t.Fatalf("err = %v", err)
	}
	fail.Store(false)
	if _, err := d.Describe(context.Background(), "vendor/plain"); err != nil {
		t.Errorf("after recovery: %v", err)
	}
}

func TestOllama(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ps" && r.Method == http.MethodGet {
			io.WriteString(w, `{"models":[]}`)
			return
		}
		if r.URL.Path != "/api/show" || r.Method != http.MethodPost {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		var body struct{ Model string }
		json.NewDecoder(r.Body).Decode(&body)
		switch body.Model {
		case "qwen3:1.7b":
			io.WriteString(w, `{"capabilities":["completion","tools","thinking"],"model_info":{"general.architecture":"qwen3","qwen3.context_length":40960}}`)
		case "qwen3-coder:30b":
			io.WriteString(w, `{"capabilities":["completion","tools"],"model_info":{"qwen3moe.context_length":262144}}`)
		default:
			http.Error(w, `{"error":"model not found"}`, http.StatusNotFound)
		}
	})
	d := Ollama(srv.URL+"/v1/", nil)
	ctx := context.Background()
	info, err := d.Describe(ctx, "qwen3:1.7b")
	if err != nil || info.Reasons != Yes || info.Efforts != nil || info.ContextWindow != 40960 || info.Source != "ollama" {
		t.Errorf("thinking model: %+v %v", info, err)
	}
	info, err = d.Describe(ctx, "qwen3-coder:30b")
	if err != nil || info.Reasons != No || !slices.Equal(info.Efforts, []E{"none"}) || info.ContextWindow != 262144 {
		t.Errorf("model without thinking: %+v %v", info, err)
	}
	if _, err := d.Describe(ctx, "nope"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("unknown model: %v", err)
	}
}

// On Ollama the window is the one the server runs the model at: a
// loaded model's context_length from /api/ps, or a num_ctx the model's
// parameters set; the trained length is only an upper bound, and the
// window when the server says nothing else (#171).
func TestOllamaTakesTheServedWindow(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			io.WriteString(w, `{"models":[{"name":"qwen3.5:9b","model":"qwen3.5:9b","context_length":65536},{"name":"tiny:1b","model":"tiny:1b","context_length":1048576}]}`)
			return
		case "/api/show":
		default:
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		var body struct{ Model string }
		json.NewDecoder(r.Body).Decode(&body)
		switch body.Model {
		case "qwen3.5:9b":
			io.WriteString(w, `{"capabilities":["completion"],"model_info":{"qwen35.context_length":262144}}`)
		case "small-ctx":
			io.WriteString(w, `{"capabilities":["completion"],"parameters":"stop \"<|im_end|>\"\nnum_ctx 2048\ntemperature 0.6","model_info":{"qwen3.context_length":40960}}`)
		case "tiny:1b":
			io.WriteString(w, `{"capabilities":["completion"],"model_info":{"llama.context_length":8192}}`)
		case "unloaded":
			io.WriteString(w, `{"capabilities":["completion"],"model_info":{"qwen3.context_length":40960}}`)
		}
	})
	d := Ollama(srv.URL+"/v1", nil)
	for _, tc := range []struct {
		model  string
		window int64
		from   string
	}{
		{"qwen3.5:9b", 65536, "ollama /api/ps"},
		{"small-ctx", 2048, "ollama num_ctx"},
		{"tiny:1b", 8192, "ollama trained length"},
		{"unloaded", 40960, "ollama trained length"},
	} {
		info, err := d.Describe(context.Background(), tc.model)
		if err != nil || info.ContextWindow != tc.window || info.WindowFrom != tc.from {
			t.Errorf("%s: window %d from %q (%v), want %d from %q", tc.model, info.ContextWindow, info.WindowFrom, err, tc.window, tc.from)
		}
	}
	info, _ := d.Describe(context.Background(), "qwen3.5:9b")
	if info.CompactBudget() != 49152 {
		t.Errorf("budget %d, want 49152", info.CompactBudget())
	}
}

func TestAnthropic(t *testing.T) {
	models := map[string]string{
		"claude-effort": `{"id":"claude-effort","type":"model","display_name":"E","created_at":"2026-01-01T00:00:00Z",
			"max_input_tokens":1000000,"max_tokens":128000,
			"capabilities":{"thinking":{"supported":true},"effort":{"supported":true,"low":{"supported":true},
			"medium":{"supported":true},"high":{"supported":true},"xhigh":{"supported":false},"max":{"supported":true}}}}`,
		"claude-thinks": `{"id":"claude-thinks","type":"model","display_name":"T","created_at":"2026-01-01T00:00:00Z",
			"max_input_tokens":200000,"max_tokens":64000,
			"capabilities":{"thinking":{"supported":true},"effort":{"supported":false}}}`,
	}
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/v1/models/")
		body, ok := models[id]
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"type":"error","error":{"type":"not_found_error","message":"model: nope"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	})
	client := sdk.NewClient(option.WithAPIKey("k"), option.WithBaseURL(srv.URL), option.WithMaxRetries(0))
	d := Anthropic(&client.Models)
	ctx := context.Background()
	info, err := d.Describe(ctx, "claude-effort")
	if err != nil || info.Reasons != Yes || !slices.Equal(info.Efforts, []E{"none", "low", "medium", "high", "max"}) ||
		info.ContextWindow != 1_000_000 || info.MaxOutput != 128_000 || info.Source != "anthropic" {
		t.Errorf("effort model: %+v %v", info, err)
	}
	// The adapter sends effort as output_config.effort, so without it
	// only none can go.
	info, err = d.Describe(ctx, "claude-thinks")
	if err != nil || info.Reasons != Yes || !slices.Equal(info.Efforts, []E{"none"}) {
		t.Errorf("model without effort: %+v %v", info, err)
	}
	if _, err := d.Describe(ctx, "nope"); err == nil {
		t.Error("unknown model: no error")
	}
}

func TestGemini(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/models/gemini-thinks"):
			io.WriteString(w, `{"name":"models/gemini-thinks","inputTokenLimit":1048576,"outputTokenLimit":65536,"thinking":true}`)
		case strings.HasSuffix(r.URL.Path, "/models/gemini-flat"):
			io.WriteString(w, `{"name":"models/gemini-flat","inputTokenLimit":32768,"outputTokenLimit":8192}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":{"code":404,"message":"not found","status":"NOT_FOUND"}}`)
		}
	})
	client, err := genai.NewClient(context.Background(), &genai.ClientConfig{APIKey: "k", Backend: genai.BackendGeminiAPI, HTTPOptions: genai.HTTPOptions{BaseURL: srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	d := Gemini(client.Models)
	ctx := context.Background()
	info, err := d.Describe(ctx, "gemini-thinks")
	if err != nil || info.Reasons != Yes || info.Efforts != nil || info.ContextWindow != 1048576 || info.MaxOutput != 65536 || info.Source != "gemini" {
		t.Errorf("thinking model: %+v %v", info, err)
	}
	info, err = d.Describe(ctx, "gemini-flat")
	if err != nil || info.Reasons != No || !slices.Equal(info.Efforts, []E{"none"}) {
		t.Errorf("model without thinking: %+v %v", info, err)
	}
	if _, err := d.Describe(ctx, "nope"); err == nil {
		t.Error("unknown model: no error")
	}
}

// The user's window wins over the vendor's, the flag's over theirs, and
// a project's only where it is lower (#171).
func TestWithWindow(t *testing.T) {
	vendor := Info{Source: "ollama", ContextWindow: 262144, WindowFrom: "ollama trained length"}
	for _, tc := range []struct {
		name             string
		user, proj, flag int64
		want             int64
		from             string
	}{
		{"none", 0, 0, 0, 262144, "ollama trained length"},
		{"the user's", 65536, 0, 0, 65536, "config"},
		{"a project's lower", 65536, 32768, 0, 32768, "project config"},
		{"a project's higher", 65536, 131072, 0, 65536, "config"},
		{"a project's lower than the vendor's", 0, 32768, 0, 32768, "project config"},
		{"the flag's", 65536, 32768, 100000, 100000, "flag"},
	} {
		got := vendor.WithWindow(tc.user, tc.proj, tc.flag)
		if got.ContextWindow != tc.want || got.WindowFrom != tc.from {
			t.Errorf("%s: %d from %q, want %d from %q", tc.name, got.ContextWindow, got.WindowFrom, tc.want, tc.from)
		}
	}
}

// #166: whether a model takes images: Ollama's vision capability,
// OpenRouter's input modalities; unknown where the vendor says nothing.
func TestImagesSupport(t *testing.T) {
	ollama := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ps" {
			io.WriteString(w, `{"models":[]}`)
			return
		}
		var body struct{ Model string }
		json.NewDecoder(r.Body).Decode(&body)
		caps := `["completion"]`
		if body.Model == "llava" {
			caps = `["completion","vision"]`
		}
		io.WriteString(w, `{"capabilities":`+caps+`}`)
	})
	o := Ollama(ollama.URL+"/v1", nil)
	for model, want := range map[string]Support{"llava": Yes, "qwen3": No} {
		if info, err := o.Describe(context.Background(), model); err != nil || info.Images != want {
			t.Errorf("ollama %s: images %v, %v; want %v", model, info.Images, err, want)
		}
	}
	or := serve(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"v","architecture":{"input_modalities":["text","image"]}},{"id":"t","architecture":{"input_modalities":["text"]}},{"id":"u"}]}`)
	})
	d := OpenRouter(or.URL, nil)
	for model, want := range map[string]Support{"v": Yes, "t": No, "u": Unknown} {
		if info, err := d.Describe(context.Background(), model); err != nil || info.Images != want {
			t.Errorf("openrouter %s: images %v, %v; want %v", model, info.Images, err, want)
		}
	}
	s := Of(Wrap(&recorder{}, d, nil))
	if !s.TakesImages(context.Background(), "v") || s.TakesImages(context.Background(), "u") {
		t.Error("TakesImages is not the vendor's yes")
	}
}
