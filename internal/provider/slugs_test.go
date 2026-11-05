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

	"github.com/ChristopherDavenport/openresponses"
)

// A provider's own API (Ollama, OpenAI, OpenRouter) refuses an item
// under another provider's slug, such as the Claude adapter's redacted
// thinking a conversation kept from before /model moved it, so the
// client leaves those out (openresponses extensions.md section 1.6). An
// openresponses server may be a proxy over the Claude adapter that
// needs them back, so it is sent everything.
func TestProviderItemsAreLeftOutForAProvidersOwnAPI(t *testing.T) {
	for _, tc := range []struct {
		provider string
		want     bool
	}{
		{"ollama", false},
		{"openresponses", true},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			var mu sync.Mutex
			var body string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				mu.Lock()
				body = string(b)
				mu.Unlock()
				http.Error(w, `{"error":{"type":"invalid_request_error","message":"no"}}`, http.StatusBadRequest)
			}))
			defer srv.Close()
			m, err := New(context.Background(), Spec{Provider: tc.provider, BaseURL: srv.URL, Model: "m"})
			if err != nil {
				t.Fatal(err)
			}
			thinking := &openresponses.UnknownItem{Type: "anthropic.redacted_thinking", Raw: json.RawMessage(`{"type":"anthropic.redacted_thinking","data":"x"}`)}
			_ = m.Streamer.CreateStream(context.Background(), openresponses.Request{Model: "m", Input: openresponses.Items{openresponses.UserText("hi"), thinking}}, discard)
			mu.Lock()
			defer mu.Unlock()
			if body == "" {
				t.Fatal("no request reached the server")
			}
			if got := strings.Contains(body, "anthropic.redacted_thinking"); got != tc.want {
				t.Errorf("sent the Claude item: %v, want %v; body %s", got, tc.want, body)
			}
		})
	}
}
