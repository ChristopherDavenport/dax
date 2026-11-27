package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/v2/option"
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
	mu    sync.Mutex
	seen  []http.Header
	paths []string
}

func newHeaderServer(t *testing.T) *headerServer {
	s := &headerServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.seen = append(s.seen, r.Header.Clone())
		s.paths = append(s.paths, r.URL.Path)
		s.mu.Unlock()
		http.Error(w, `{"error":{"type":"invalid_request_error","message":"no"}}`, http.StatusBadRequest)
	}))
	t.Cleanup(s.Close)
	return s
}

// lastPath is the path of the latest request.
func (s *headerServer) lastPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.paths) == 0 {
		return ""
	}
	return s.paths[len(s.paths)-1]
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
			c := anthropicClient(spec, "", keys, option.WithBaseURL(url))
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

// The anthropic provider with a base_url sends to /v1/messages on that
// server, with the key as a bearer token whether it comes from the
// environment or from the key command, and with the session and client
// headers.
func TestAnthropicBaseURLIsTheServerSent(t *testing.T) {
	const sid = "019a0000-0000-7000-8000-0000000000bb"
	for _, tc := range []struct {
		name    string
		command bool
		want    string
	}{
		{"environment key", false, "Bearer env-key"},
		{"key command", true, "Bearer gw-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newHeaderServer(t)
			spec := Spec{
				Provider: "anthropic", BaseURL: srv.URL, Getenv: env(map[string]string{"ANTHROPIC_API_KEY": "env-key"}),
				SessionHeader: "X-Session-Id", ClientHeader: "X-Client", Client: "dax/v1.2.3",
			}
			if tc.command {
				spec.KeyCommand = helper(t, "gw-key")
			}
			m, err := New(context.Background(), spec)
			if err != nil {
				t.Fatal(err)
			}
			if m.Endpoint != srv.URL {
				t.Errorf("endpoint %q, want %q", m.Endpoint, srv.URL)
			}
			ctx := session.ContextWithSessionID(context.Background(), sid)
			// The test server answers 400, so an error is expected; it must
			// be that answer, not a failure to reach the server.
			err = m.Streamer.CreateStream(ctx, openresponses.Request{Model: "claude-sonnet-5-5", Input: openresponses.Items{openresponses.UserText("hi")}}, discard)
			if err == nil || !strings.Contains(err.Error(), "400") {
				t.Errorf("CreateStream error = %v, want the 400 the server sent", err)
			}
			h := srv.last(t)
			if path := srv.lastPath(); path != "/v1/messages" {
				t.Errorf("path %q, want /v1/messages", path)
			}
			if got := h.Get("Authorization"); got != tc.want {
				t.Errorf("authorization %q, want %q", got, tc.want)
			}
			if got := h.Get("X-Api-Key"); got != "" {
				t.Errorf("x-api-key %q, want none", got)
			}
			if got := h.Get("X-Session-Id"); got != sid {
				t.Errorf("session header %q, want %q", got, sid)
			}
			if got := h.Get("X-Client"); got != "dax/v1.2.3" {
				t.Errorf("client header %q", got)
			}
		})
	}
}

// A base_url ending in /v1 would be sent to /v1/v1/messages, so it is
// refused; any other URL is used as given.
func TestAnthropicBaseURLRefusesATrailingV1(t *testing.T) {
	for _, tc := range []struct {
		base string
		ok   bool
	}{
		{"https://proxy.example/anthropic", true},
		{"https://proxy.example/anthropic/", true},
		{"https://proxy.example", true},
		{"https://proxy.example/v1", false},
		{"https://proxy.example/v1/", false},
		{"https://proxy.example/anthropic/v1", false},
		{"https://proxy.example/v10", true},
	} {
		_, err := New(context.Background(), Spec{Provider: "anthropic", BaseURL: tc.base, Getenv: env(map[string]string{"ANTHROPIC_API_KEY": "k"})})
		if tc.ok && err != nil {
			t.Errorf("%s: %v", tc.base, err)
		}
		if !tc.ok && (err == nil || !strings.Contains(err.Error(), "/v1")) {
			t.Errorf("%s: err = %v, want a refusal naming /v1", tc.base, err)
		}
	}
}
