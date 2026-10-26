package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/ChristopherDavenport/openresponses/providers/anthropic"
)

// keyRecorder is an adapter that notes the prompt cache key of each
// request it is sent.
type keyRecorder struct {
	openresponses.UnsupportedCompaction
	got *[]string
}

func (r keyRecorder) Create(_ context.Context, req openresponses.Request) (*openresponses.Response, error) {
	*r.got = append(*r.got, req.PromptCacheKey)
	return &openresponses.Response{}, nil
}

func (r keyRecorder) CreateStream(_ context.Context, req openresponses.Request, _ openresponses.EventSink) error {
	*r.got = append(*r.got, req.PromptCacheKey)
	return nil
}

// Every request to a Claude adapter, on its own or on vertex's Claude
// route, by either method, carries a cache key: its own, else the run's
// session, else the fixed one. Vertex's Gemini route gets none.
func TestClaudeRequestsCarryACacheKey(t *testing.T) {
	const parent, child = "019a0000-0000-7000-8000-000000000001", "019a0000-0000-5000-8000-000000000002"
	run := session.ContextWithSessionID(context.Background(), parent)
	sub := session.ContextWithSessionID(run, child)
	for _, tc := range []struct {
		name  string
		ctx   context.Context
		model string
		key   string
		want  string
	}{
		{"the run's session", run, "claude-opus-5-5", "", parent},
		{"a sub-agent's own session", sub, "claude-opus-5-5", "", child},
		{"no session outside a run", context.Background(), "claude-opus-5-5", "", noSessionCacheKey},
		{"a key of the request's own", run, "claude-opus-5-5", "mine", "mine"},
		{"a key of its own outside a run", context.Background(), "claude-opus-5-5", "mine", "mine"},
		{"gemini on vertex", run, "gemini-2.5-pro", "", ""},
		{"gemini on vertex outside a run", context.Background(), "gemini-2.5-pro", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var claude, gemini []string
			paths := map[string]openresponses.Streamer{
				"vertex": vertexModels{claude: withClaudeCache(keyRecorder{got: &claude}), gemini: keyRecorder{got: &gemini}},
			}
			if vertexFamily(tc.model) == familyClaude {
				paths["anthropic"] = withClaudeCache(keyRecorder{got: &claude})
			}
			for name, s := range paths {
				claude, gemini = nil, nil
				req := openresponses.Request{Model: tc.model, PromptCacheKey: tc.key}
				if err := s.CreateStream(tc.ctx, req, nil); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if a, ok := s.(openresponses.Adapter); ok {
					if _, err := a.Create(tc.ctx, req); err != nil {
						t.Fatalf("%s: %v", name, err)
					}
				}
				got := append(claude, gemini...)
				if len(got) == 0 {
					t.Fatalf("%s: no request reached an adapter", name)
				}
				for _, key := range got {
					if key != tc.want {
						t.Errorf("%s: cache key %q, want %q", name, key, tc.want)
					}
				}
			}
		})
	}
}

// The wrapper keeps the adapter's compaction answer, so -compact-server
// and withKeyErrors see the same model.
func TestClaudeCacheKeepsTheAdapter(t *testing.T) {
	c := withClaudeCache(anthropic.New(sdk.NewClient(option.WithAPIKey("k")).Messages))
	if _, err := c.Compact(context.Background(), openresponses.CompactRequest{Model: "claude-opus-5-5"}); err == nil {
		t.Error("compaction should be unsupported, as the adapter's is")
	}
	if _, ok := withKeyErrors(c, newFakeKeys().keySource).(compactor); !ok {
		t.Error("withKeyErrors hid the adapter's Compact")
	}
}

// What the key buys: the anthropic adapter puts automatic caching on
// the Messages request it sends for a session's turn.
func TestClaudeRequestOptsIntoCaching(t *testing.T) {
	var (
		mu   sync.Mutex
		body map[string]json.RawMessage
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		_ = json.Unmarshal(b, &body)
		mu.Unlock()
		http.Error(w, `{"error":{"type":"invalid_request_error","message":"no"}}`, http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	client := sdk.NewClient(option.WithAPIKey("k"), option.WithBaseURL(srv.URL), option.WithMaxRetries(0))
	m := withClaudeCache(anthropic.New(client.Messages))
	ctx := session.ContextWithSessionID(context.Background(), "019a0000-0000-7000-8000-0000000000aa")
	_ = m.CreateStream(ctx, openresponses.Request{Model: "claude-opus-5-5", Input: openresponses.Items{openresponses.UserText("hi")}}, discard)
	mu.Lock()
	defer mu.Unlock()
	if body == nil {
		t.Fatal("no request reached the server")
	}
	var cc struct{ Type string }
	if err := json.Unmarshal(body["cache_control"], &cc); err != nil || cc.Type != "ephemeral" {
		t.Errorf("cache_control = %s, want an ephemeral one", body["cache_control"])
	}
}
