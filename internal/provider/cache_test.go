package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/ChristopherDavenport/openresponses/providers/anthropic"
	"github.com/ChristopherDavenport/openresponses/providers/gemini"
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

// Each TTL reaches the Messages request its own streamer sends: the
// main conversation's through the main adapter, the sub-agents'
// through theirs, and one adapter serves both when they ask the same.
func TestCacheTTLsReachTheWire(t *testing.T) {
	var (
		mu  sync.Mutex
		ttl *string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body struct {
			CacheControl *struct {
				TTL string `json:"ttl"`
			} `json:"cache_control"`
		}
		_ = json.Unmarshal(b, &body)
		mu.Lock()
		got := "none"
		if body.CacheControl != nil {
			got = body.CacheControl.TTL
		}
		ttl = &got
		mu.Unlock()
		http.Error(w, `{"error":{"type":"invalid_request_error","message":"no"}}`, http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	client := sdk.NewClient(option.WithAPIKey("k"), option.WithBaseURL(srv.URL), option.WithMaxRetries(0))
	send := func(t *testing.T, s openresponses.Streamer) string {
		t.Helper()
		mu.Lock()
		ttl = nil
		mu.Unlock()
		ctx := session.ContextWithSessionID(context.Background(), "019a0000-0000-7000-8000-0000000000aa")
		_ = s.CreateStream(ctx, openresponses.Request{Model: "claude-opus-5-5", Input: openresponses.Items{openresponses.UserText("hi")}}, discard)
		mu.Lock()
		defer mu.Unlock()
		if ttl == nil {
			t.Fatal("no request reached the server")
		}
		return *ttl
	}
	for _, tc := range []struct {
		main, sub         string
		separate          bool
		wantMain, wantSub string // "" is an ephemeral cache_control with no ttl, Claude's 5 minutes
	}{
		{"", "", false, "", ""},
		{"5m", "", false, "", ""},
		{"1h", "1h", false, "1h", "1h"},
		{"1h", "", true, "1h", ""},
		{"1h", "5m", true, "1h", ""},
		{"", "1h", true, "", "1h"},
	} {
		t.Run(tc.main+"/"+tc.sub, func(t *testing.T) {
			main, sub, separate := claudeAdapters(client.Messages, Spec{CacheTTL: tc.main, SubagentCacheTTL: tc.sub})
			if separate != tc.separate {
				t.Fatalf("separate = %v, want %v", separate, tc.separate)
			}
			if got := send(t, main); got != tc.wantMain {
				t.Errorf("main conversation's ttl %q, want %q", got, tc.wantMain)
			}
			if got := send(t, sub); got != tc.wantSub {
				t.Errorf("sub-agents' ttl %q, want %q", got, tc.wantSub)
			}
		})
	}
}

// New gives the sub-agents a streamer of their own only when their TTL
// differs, on anthropic and on vertex's Claude route, and refuses a
// TTL elsewhere or one Claude does not offer.
func TestNewCacheTTLs(t *testing.T) {
	env := map[string]string{"ANTHROPIC_API_KEY": "k", "OPENAI_API_KEY": "k", ProjectEnv: "p", LocationEnv: "global"}
	build := func(spec Spec) (Model, error) {
		spec.Getenv = func(k string) string { return env[k] }
		spec.Credentials = fakeCredentials
		return New(context.Background(), spec)
	}
	for _, tc := range []struct {
		spec     Spec
		separate bool
	}{
		{Spec{Provider: "anthropic"}, false},
		{Spec{Provider: "anthropic", CacheTTL: "1h", SubagentCacheTTL: "1h"}, false},
		{Spec{Provider: "anthropic", CacheTTL: "1h"}, true},
		{Spec{Provider: "vertex", CacheTTL: "1h", SubagentCacheTTL: "5m"}, true},
		{Spec{Provider: "vertex", SubagentCacheTTL: "5m"}, false},
	} {
		t.Run(tc.spec.Provider+"/"+tc.spec.CacheTTL+"/"+tc.spec.SubagentCacheTTL, func(t *testing.T) {
			m, err := build(tc.spec)
			if err != nil {
				t.Fatal(err)
			}
			if (m.SubagentStreamer != nil) != tc.separate {
				t.Fatalf("sub-agent streamer %T, want one of their own: %v", m.SubagentStreamer, tc.separate)
			}
			if !tc.separate {
				return
			}
			switch s := m.SubagentStreamer.(type) {
			case claudeCache:
			case vertexModels:
				if _, ok := s.claude.(claudeCache); !ok {
					t.Errorf("vertex sub-agents' Claude route is %T, want claudeCache", s.claude)
				}
				if _, ok := s.gemini.(*gemini.Adapter); !ok {
					t.Errorf("vertex sub-agents' Gemini route is %T", s.gemini)
				}
			default:
				t.Errorf("sub-agent streamer is %T", m.SubagentStreamer)
			}
		})
	}
	for _, tc := range []struct {
		spec Spec
		want string
	}{
		{Spec{Provider: "openai", CacheTTL: "1h"}, "openai: cache_ttl is for Claude's prompt cache"},
		{Spec{Provider: "anthropic", SubagentCacheTTL: "2h"}, `subagent_cache_ttl "2h": want 5m or 1h`},
	} {
		if _, err := build(tc.spec); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: error %v, want %q", tc.spec, err, tc.want)
		}
	}
}
