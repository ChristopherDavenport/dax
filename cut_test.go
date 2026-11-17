package dax

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"google.golang.org/genai"

	"github.com/ChristopherDavenport/openresponses"
	"github.com/ChristopherDavenport/openresponses/providers/anthropic"
	"github.com/ChristopherDavenport/openresponses/providers/gemini"
)

// claudePaused is a Messages API that pauses every turn (pause_turn),
// so the adapter's continuations run out; requests counts the turns.
func claudePaused(t *testing.T, requests *atomic.Int32) openresponses.Streamer {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range []struct{ name, data string }{
			{"message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_%d","type":"message","role":"assistant","model":"claude-sonnet-5-5","content":[],"stop_reason":null,"usage":{"input_tokens":3,"output_tokens":0}}}`, n)},
			{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Once upon"}}`},
			{"content_block_stop", `{"type":"content_block_stop","index":0}`},
			{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"pause_turn"},"usage":{"output_tokens":2}}`},
			{"message_stop", `{"type":"message_stop"}`},
		} {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.name, ev.data)
		}
	}))
	t.Cleanup(srv.Close)
	c := sdk.NewClient(option.WithAPIKey("k"), option.WithBaseURL(srv.URL), option.WithMaxRetries(0))
	return anthropic.New(c.Messages)
}

// geminiCut is a Gemini API that cuts every answer with finish reason
// CONTINUATION and no token to continue it with.
func geminiCut(t *testing.T, requests *atomic.Int32) openresponses.Streamer {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Once upon"}]},"finishReason":"CONTINUATION","index":0}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5},"modelVersion":"gemini-2.5-pro"}`+"\r\n\r\n")
	}))
	t.Cleanup(srv.Close)
	c, err := genai.NewClient(t.Context(), &genai.ClientConfig{APIKey: "k", Backend: genai.BackendGeminiAPI, HTTPOptions: genai.HTTPOptions{BaseURL: srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	return gemini.New(c)
}

// An answer the provider cut and its adapter did not continue to the
// end is cut under -p, exit 7, with the provider's reason in -result's
// details (#212): Claude's turn still paused (pause_turn) once the
// adapter's continuations ran out, which used to finish done with what
// it had, and Gemini's CONTINUATION with no token, which used to fail
// the run with model_error (openresponses providers v0.0.22).
func TestPrintSaysAProvidersCut(t *testing.T) {
	for _, tc := range []struct {
		name     string
		streamer func(*testing.T, *atomic.Int32) openresponses.Streamer
		// detail and value are the provider's reason in the details;
		// requests is how many the run sends: Claude's first turn and
		// the adapter's eight continuations, Gemini's one.
		detail, value string
		requests      int32
	}{
		{"anthropic pause_turn", claudePaused, "anthropic.stop_reason", "pause_turn", 1 + anthropic.DefaultContinuations},
		{"gemini CONTINUATION", geminiCut, "gemini.finish_reason", "CONTINUATION", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			sess, _ := sessionOver(t, tc.streamer(t, &requests))
			f := &printFront{info: frontInfo{Name: "dax", Prompt: "go"}, in: bufio.NewScanner(strings.NewReader(""))}
			var ended *runEnded
			if err := f.run(t.Context(), sess, sess); !errors.As(err, &ended) || ended.status != exitCut {
				t.Fatalf("run: %v, want exit %d", err, exitCut)
			}
			if n := requests.Load(); n != tc.requests {
				t.Errorf("%d requests, want %d", n, tc.requests)
			}
			again, _ := sessionOver(t, tc.streamer(t, &requests))
			path := filepath.Join(t.TempDir(), "result.json")
			f = &printFront{info: frontInfo{Name: "dax", Prompt: "go", Result: path}, in: bufio.NewScanner(strings.NewReader(""))}
			if err := f.run(t.Context(), again, again); err != nil {
				t.Fatalf("run with -result: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Reason, Cause string
				Exit          int
				Incomplete    map[string]any
			}
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if got.Reason != "cut" || got.Cause != "max_output_tokens" || got.Exit != exitCut || got.Incomplete["reason"] != "max_output_tokens" || got.Incomplete[tc.detail] != tc.value {
				t.Errorf("result %s, want cut, max_output_tokens, exit %d and %s %s", data, exitCut, tc.detail, tc.value)
			}
		})
	}
}
