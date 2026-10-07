package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"google.golang.org/genai"

	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/ChristopherDavenport/openresponses/providers/anthropic"
	"github.com/ChristopherDavenport/openresponses/providers/gemini"
)

// headerServer records the session, client and authorization headers of
// each request it is sent.
type headerServer struct {
	*httptest.Server
	mu   sync.Mutex
	seen []http.Header
}

func newHeaderServer(t *testing.T) *headerServer {
	s := &headerServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.seen = append(s.seen, r.Header.Clone())
		s.mu.Unlock()
		http.Error(w, `{"error":{"type":"invalid_request_error","message":"no"}}`, http.StatusBadRequest)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *headerServer) last(t *testing.T) http.Header {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.seen) == 0 {
		t.Fatal("no request reached the server")
	}
	return s.seen[len(s.seen)-1]
}

func TestHeaderTransport(t *testing.T) {
	const parent, child = "019a0000-0000-7000-8000-000000000001", "019a0000-0000-5000-8000-000000000002"
	for _, tc := range []struct {
		name                    string
		sessionHeader, client   string
		ctx                     context.Context
		wantSession, wantClient string
	}{
		{"the run's session", "X-Session-Id", "X-Client", session.ContextWithSessionID(context.Background(), parent), parent, "dax/v1.2.3"},
		{"a sub-agent's own session", "X-Session-Id", "", session.ContextWithSessionID(session.ContextWithSessionID(context.Background(), parent), child), child, ""},
		{"no session outside a run", "X-Session-Id", "X-Client", context.Background(), "", "dax/v1.2.3"},
		{"only the client", "", "X-Client", session.ContextWithSessionID(context.Background(), parent), "", "dax/v1.2.3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newHeaderServer(t)
			rt := transport(Spec{SessionHeader: tc.sessionHeader, ClientHeader: tc.client, Client: "dax/v1.2.3"}, nil, nil, nil)
			req, _ := http.NewRequestWithContext(tc.ctx, http.MethodGet, srv.URL, nil)
			res, err := (&http.Client{Transport: rt}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = res.Body.Close()
			h := srv.last(t)
			if got := h.Get("X-Session-Id"); got != tc.wantSession {
				t.Errorf("session header %q, want %q", got, tc.wantSession)
			}
			if got := h.Get("X-Client"); got != tc.wantClient {
				t.Errorf("client header %q, want %q", got, tc.wantClient)
			}
			if len(req.Header) != 0 {
				t.Errorf("the caller's request was changed: %v", req.Header)
			}
		})
	}
	if transport(Spec{}, nil, nil, nil) != nil {
		t.Error("with nothing to add, transport should leave the client its own")
	}
}

// Each vendor client sends the session of the model call's run and the
// client under the names given, beside the command's key.
func TestClientsSendSessionAndClient(t *testing.T) {
	const sid = "019a0000-0000-7000-8000-0000000000aa"
	spec := Spec{SessionHeader: "X-Session-Id", ClientHeader: "X-Client", Client: "dax/v1.2.3"}
	for _, tc := range []struct {
		name  string
		build func(t *testing.T, keys *keySource, url string) openresponses.Streamer
	}{
		{"openresponses", func(t *testing.T, keys *keySource, url string) openresponses.Streamer {
			spec := spec
			spec.Provider, spec.BaseURL, spec.Model, spec.KeyCommand = "openresponses", url, "m", helper(t, "key-1")
			m, err := New(context.Background(), spec)
			if err != nil {
				t.Fatal(err)
			}
			return m.Streamer
		}},
		{"anthropic", func(t *testing.T, keys *keySource, url string) openresponses.Streamer {
			c := sdk.NewClient(append(anthropicOptions("", keys, transport(spec, keys, xAPIKey, nil)), option.WithBaseURL(url))...)
			return anthropic.New(c.Messages)
		}},
		{"gemini", func(t *testing.T, keys *keySource, url string) openresponses.Streamer {
			cc := geminiConfig("", keys, transport(spec, keys, googAPIKey, nil))
			cc.HTTPOptions.BaseURL = url
			c, err := genai.NewClient(context.Background(), cc)
			if err != nil {
				t.Fatal(err)
			}
			return gemini.New(c)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newHeaderServer(t)
			m := tc.build(t, newFakeKeys().keySource, srv.URL)
			ctx := session.ContextWithSessionID(context.Background(), sid)
			_ = m.CreateStream(ctx, openresponses.Request{Model: "m", Input: openresponses.Items{openresponses.UserText("hi")}}, discard)
			h := srv.last(t)
			if got := h.Get("X-Session-Id"); got != sid {
				t.Errorf("session header %q, want %q", got, sid)
			}
			if got := h.Get("X-Client"); got != "dax/v1.2.3" {
				t.Errorf("client header %q, want dax/v1.2.3", got)
			}
			if key := bearerOf(&http.Request{Header: h}) + h.Get("X-Api-Key") + h.Get("X-Goog-Api-Key"); key != "key-1" {
				t.Errorf("key sent %q, want the command's key-1 alone", key)
			}
		})
	}
}

func TestVertexRefusesTheHeaders(t *testing.T) {
	_, err := New(context.Background(), Spec{Provider: "vertex", SessionHeader: "X-Session-Id", Getenv: func(string) string { return "global" }})
	if err == nil || !strings.Contains(err.Error(), "session_header") {
		t.Errorf("err = %v, want vertex to refuse session_header", err)
	}
}
