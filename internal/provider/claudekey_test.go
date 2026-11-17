package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

// claudeKeyOf is the key a request to Claude carries: Authorization's
// bearer token, as anthropic-sdk-go v2 sends every credential, and
// "both headers" when an X-Api-Key goes beside it or alone.
func claudeKeyOf(r *http.Request) string {
	if r.Header.Get("X-Api-Key") != "" {
		return "both headers"
	}
	return bearerOf(r)
}

// Claude's key goes in one header, Authorization: Bearer, whether it
// came from ANTHROPIC_API_KEY or from api_key_command (#212). SDK v2,
// at API version 2026-10-21, sends an API key and an auth token alike
// as a bearer token, and a token a key command prints that is not an
// API key is accepted there alone. The SDK's own variables add no
// second credential on either path.
func TestClaudeKeyHeader(t *testing.T) {
	for _, tc := range []struct {
		name   string
		client func(url string) sdk.Client
		want   string
	}{
		{"environment", func(url string) sdk.Client {
			return anthropicClient(Spec{}, "env-key", nil, option.WithBaseURL(url))
		}, "env-key"},
		{"key command", func(url string) sdk.Client {
			return anthropicClient(Spec{}, "", newFakeKeys().keySource, option.WithBaseURL(url))
		}, "key-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ANTHROPIC_API_KEY", "from-sdk-env")
			t.Setenv("ANTHROPIC_AUTH_TOKEN", "from-sdk-env")
			var (
				mu   sync.Mutex
				seen []http.Header
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				seen = append(seen, r.Header.Clone())
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[],"has_more":false}`))
			}))
			t.Cleanup(srv.Close)
			c := tc.client(srv.URL)
			if _, err := c.Models.List(context.Background(), sdk.ModelListParams{}); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(seen) != 1 {
				t.Fatalf("%d requests, want 1", len(seen))
			}
			h := seen[0]
			if got := h.Get("Authorization"); got != "Bearer "+tc.want {
				t.Errorf("Authorization %q, want Bearer %s", got, tc.want)
			}
			if got := h.Values("X-Api-Key"); len(got) != 0 {
				t.Errorf("X-Api-Key %q, want none", got)
			}
		})
	}
}
