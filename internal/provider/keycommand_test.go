package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"google.golang.org/genai"

	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/ChristopherDavenport/openresponses/providers/anthropic"
	"github.com/ChristopherDavenport/openresponses/providers/gemini"
)

// The test binary is its own key command: run with keyHelperEnv set,
// it prints that variable's value and exits, so no test needs a shell
// or a program on the PATH.
const keyHelperEnv = "DAX_TEST_KEY_HELPER"

func TestMain(m *testing.M) {
	if out, ok := os.LookupEnv(keyHelperEnv); ok {
		if out == "fail" {
			fmt.Fprintln(os.Stderr, "helper: refusing")
			os.Exit(3)
		}
		fmt.Print(out)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// helper is a key command that prints out.
func helper(t *testing.T, out string) []string {
	t.Helper()
	t.Setenv(keyHelperEnv, out)
	return []string{os.Args[0], "-test.run=^$"}
}

// fakeKeys is a key source whose command hands out key-1, key-2, ...
// in turn, on a clock the test moves.
type fakeKeys struct {
	*keySource
	runs  atomic.Int32
	fails int
	clock time.Time
}

func newFakeKeys() *fakeKeys {
	f := &fakeKeys{clock: time.Unix(1_000_000, 0)}
	f.keySource = &keySource{argv: []string{"helper"}, ttl: KeyTTL, now: func() time.Time { return f.clock }}
	f.run = func(context.Context, []string) (string, error) {
		return fmt.Sprintf("key-%d", f.runs.Add(1)), nil
	}
	return f
}

func TestKeyIsCachedForTheTTL(t *testing.T) {
	f := newFakeKeys()
	ctx := context.Background()
	for _, tc := range []struct {
		advance time.Duration
		want    string
	}{
		{0, "key-1"},
		{time.Second, "key-1"},
		{KeyTTL - 2*time.Second, "key-1"},
		{time.Second, "key-2"},
		{time.Minute, "key-2"},
	} {
		f.clock = f.clock.Add(tc.advance)
		if got, err := f.Key(ctx); err != nil || got != tc.want {
			t.Fatalf("after %v: %q, %v; want %q", tc.advance, got, err, tc.want)
		}
	}
	if n := f.runs.Load(); n != 2 {
		t.Errorf("command ran %d times, want 2", n)
	}
}

func TestRefresh(t *testing.T) {
	f := newFakeKeys()
	ctx := context.Background()
	k1, _ := f.Key(ctx)
	// The refused key is replaced at once, TTL or no.
	if k2, err := f.Refresh(ctx, k1); err != nil || k2 != "key-2" {
		t.Fatalf("refresh of the current key: %q, %v", k2, err)
	}
	// A caller still holding the old key gets the replacement without
	// another run.
	if k, err := f.Refresh(ctx, k1); err != nil || k != "key-2" {
		t.Fatalf("refresh of a key already replaced: %q, %v", k, err)
	}
	if n := f.runs.Load(); n != 2 {
		t.Errorf("command ran %d times, want 2", n)
	}
}

func TestConcurrentCallersShareOneRun(t *testing.T) {
	f := newFakeKeys()
	release := make(chan struct{})
	run := f.run
	f.run = func(ctx context.Context, argv []string) (string, error) {
		<-release
		return run(ctx, argv)
	}
	var wg sync.WaitGroup
	got := make([]string, 8)
	for i := range got {
		wg.Go(func() { got[i], _ = f.Key(context.Background()) })
	}
	// Let every caller reach the wait before the run finishes.
	for f.pendingWaiters() == nil {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(10 * time.Millisecond)
	close(release)
	wg.Wait()
	for i, k := range got {
		if k != "key-1" {
			t.Errorf("caller %d: %q", i, k)
		}
	}
	if n := f.runs.Load(); n != 1 {
		t.Errorf("command ran %d times, want 1", n)
	}
}

// pendingWaiters is the run in progress, nil when there is none.
func (s *keySource) pendingWaiters() *fetch {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending
}

func TestACallerGivingUpDoesNotCancelTheRun(t *testing.T) {
	f := newFakeKeys()
	release := make(chan struct{})
	run := f.run
	f.run = func(ctx context.Context, argv []string) (string, error) {
		<-release
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return run(ctx, argv)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.Key(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller: %v", err)
	}
	close(release)
	if k, err := f.Key(context.Background()); err != nil || k != "key-1" {
		t.Fatalf("next caller: %q, %v", k, err)
	}
	if n := f.runs.Load(); n != 1 {
		t.Errorf("command ran %d times, want 1", n)
	}
}

func TestAFailedRunIsNotCached(t *testing.T) {
	f := newFakeKeys()
	fail := true
	run := f.run
	f.run = func(ctx context.Context, argv []string) (string, error) {
		if fail {
			f.fails++
			return "", &KeyError{Program: "helper", Reason: "exit status 1"}
		}
		return run(ctx, argv)
	}
	if _, err := f.Key(context.Background()); !errors.Is(err, ErrKeyCommand) {
		t.Fatalf("err = %v", err)
	}
	fail = false
	// For a moment the failure is the answer, so the retries of one
	// request do not run the command again each...
	f.clock = f.clock.Add(keyFailHold - time.Second)
	if _, err := f.Refresh(context.Background(), ""); !errors.Is(err, ErrKeyCommand) || f.fails != 1 {
		t.Fatalf("within the hold: %v, %d failed runs", err, f.fails)
	}
	// ...and then it is run again.
	f.clock = f.clock.Add(time.Second)
	if k, err := f.Key(context.Background()); err != nil || k != "key-1" {
		t.Fatalf("after the failure: %q, %v", k, err)
	}
}

func TestRunKeyCommand(t *testing.T) {
	const secret = "sk-very-secret-value"
	for _, tc := range []struct {
		name, out, want, wantErr string
	}{
		{"one line", secret + "\n", secret, ""},
		{"surrounding space", "  " + secret + " \r\n", secret, ""},
		{"nothing", "", "", "printed no key"},
		{"blank", " \n\n", "", "printed no key"},
		{"two lines", secret + "\nmore\n", "", "printed more than one line"},
		{"a space", secret + " " + secret, "", "printed something besides the key"},
		{"not ascii", secret + "é", "", "printed something besides the key"},
		{"too long", strings.Repeat(secret, maxKey/len(secret)+1), "", "printed more than"},
		{"failure", "fail", "", "exit status 3: helper: refusing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := runKeyCommand(context.Background(), helper(t, tc.out), io.Discard)
			if tc.wantErr == "" {
				if err != nil || got != tc.want {
					t.Fatalf("%q, %v; want %q", got, err, tc.want)
				}
				return
			}
			if !errors.Is(err, ErrKeyCommand) || !strings.Contains(err.Error(), tc.wantErr) || got != "" {
				t.Fatalf("%q, %v; want an error containing %q", got, err, tc.wantErr)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "-test.run") {
				t.Errorf("error carries the output or the arguments: %v", err)
			}
		})
	}
	if _, err := runKeyCommand(context.Background(), []string{"/nonexistent/key-helper"}, nil); !errors.Is(err, ErrKeyCommand) {
		t.Errorf("missing program: %v", err)
	}
}

func TestKeyCommandStderr(t *testing.T) {
	// Echoed as it is written when there is somewhere to echo it...
	var echo strings.Builder
	_, err := runKeyCommand(context.Background(), helper(t, "fail"), &echo)
	if echo.String() != "helper: refusing\n" {
		t.Errorf("echoed %q", echo.String())
	}
	var ke *KeyError
	if !errors.As(err, &ke) || ke.Detail != "helper: refusing" {
		t.Fatalf("err = %#v", err)
	}
	// ...and kept for the error either way.
	if _, err = runKeyCommand(context.Background(), helper(t, "fail"), nil); !errors.As(err, &ke) || ke.Detail != "helper: refusing" {
		t.Fatalf("not echoed: %#v", err)
	}
}

func TestTailLastLine(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"one\n", "one"},
		{"first\nlast\n\n  \n", "last"},
		{"no newline", "no newline"},
		{"\x1b[31mred\x1b[0m\r\n", "[31mred[0m"},
		{strings.Repeat("x", 300), strings.Repeat("x", 200) + "…"},
	} {
		tl := &tail{max: stderrTail}
		_, _ = io.WriteString(tl, tc.in)
		if got := tl.lastLine(); got != tc.want {
			t.Errorf("%q: %q, want %q", tc.in, got, tc.want)
		}
	}
	tl := &tail{max: 8}
	_, _ = io.WriteString(tl, "0123456789abcdef")
	if got := tl.lastLine(); got != "89abcdef" {
		t.Errorf("kept %q", got)
	}
}

func TestKeyErrorMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  *KeyError
		is   error
		want string
	}{
		{"failed", &KeyError{Program: "my-token-helper", Reason: "exit status 1", Detail: "session expired"}, ErrKeyCommand,
			"authentication failed (api key command my-token-helper: exit status 1: session expired); this usually means signing in to the provider again"},
		{"failed, with login", &KeyError{Program: "my-token-helper", Reason: "exit status 1", Login: "my-token-helper login"}, ErrKeyCommand,
			"authentication failed; sign in with my-token-helper login, then try again (api key command my-token-helper: exit status 1)"},
		{"refused", &KeyError{Program: "my-token-helper", Status: 403, Detail: "permission denied", Login: "https://example.com/sign-in"}, ErrKeyRefused,
			"authentication failed; sign in with https://example.com/sign-in, then try again (the server refused the key from my-token-helper with 403: permission denied)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("\n got %s\nwant %s", got, tc.want)
			}
			if !errors.Is(tc.err, tc.is) {
				t.Errorf("not %v", tc.is)
			}
			if tc.is == ErrKeyCommand && errors.Is(tc.err, ErrKeyRefused) || tc.is == ErrKeyRefused && errors.Is(tc.err, ErrKeyCommand) {
				t.Error("is both")
			}
		})
	}
}

// keyServer accepts only the key valid holds, answering 401 to any
// other, and records each request's key and body.
type keyServer struct {
	*httptest.Server
	mu     sync.Mutex
	valid  string
	refuse int // the status for a key not valid; zero is 401
	header func(*http.Request) string
	seen   []string
	bodies []string
}

func newKeyServer(t *testing.T, valid string, header func(*http.Request) string) *keyServer {
	s := &keyServer{valid: valid, header: header}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		key := s.header(r)
		s.seen, s.bodies = append(s.seen, key), append(s.bodies, string(body))
		ok := key == s.valid
		s.mu.Unlock()
		if !ok {
			status := s.refuse
			if status == 0 {
				status = http.StatusUnauthorized
			}
			http.Error(w, `{"error":"unauthorized"}`, status)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(s.Close)
	return s
}

func bearerOf(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func TestTransportRetriesOnceWithAFreshKey(t *testing.T) {
	for _, tc := range []struct {
		name      string
		valid     string
		refuse    int
		body      func(url string) *http.Request
		status    int
		wantSeen  []string
		wantFresh bool
	}{
		{"key accepted", "key-1", 0, get, 200, []string{"key-1"}, false},
		{"refused, replaced", "key-2", 0, post, 200, []string{"key-1", "key-2"}, true},
		{"forbidden, replaced", "key-2", 403, post, 200, []string{"key-1", "key-2"}, true},
		{"refused, no body", "key-2", 0, get, 200, []string{"key-1", "key-2"}, true},
		{"refused twice", "never", 0, post, 401, []string{"key-1", "key-2"}, true},
		{"forbidden twice", "never", 403, post, 403, []string{"key-1", "key-2"}, true},
		// A body that cannot be read twice is not sent twice.
		{"refused, body not replayable", "key-2", 0, streamed, 401, []string{"key-1"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newKeyServer(t, tc.valid, bearerOf)
			srv.refuse = tc.refuse
			f := newFakeKeys()
			client := &http.Client{Transport: keyTransport{keys: f.keySource, set: bearer}.wrap(nil)}
			req := tc.body(srv.URL)
			res, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = res.Body.Close()
			if res.StatusCode != tc.status {
				t.Errorf("status %d, want %d", res.StatusCode, tc.status)
			}
			if fmt.Sprint(srv.seen) != fmt.Sprint(tc.wantSeen) {
				t.Errorf("keys sent %v, want %v", srv.seen, tc.wantSeen)
			}
			for i, b := range srv.bodies {
				if req.Method == http.MethodPost && b != "payload" {
					t.Errorf("request %d body %q", i, b)
				}
			}
			if req.Header.Get("Authorization") != "" {
				t.Error("the caller's request was changed")
			}
			if fresh := f.runs.Load() > 1; fresh != tc.wantFresh {
				t.Errorf("command ran %d times", f.runs.Load())
			}
		})
	}
}

func get(url string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, url, nil)
	return r
}

func post(url string) *http.Request {
	r, _ := http.NewRequest(http.MethodPost, url, strings.NewReader("payload"))
	return r
}

func streamed(url string) *http.Request {
	r, _ := http.NewRequest(http.MethodPost, url, io.NopCloser(strings.NewReader("payload")))
	return r
}

func TestTransportSurfacesACommandFailure(t *testing.T) {
	f := newFakeKeys()
	f.run = func(context.Context, []string) (string, error) {
		return "", &KeyError{Program: "helper", Reason: "exit status 1"}
	}
	srv := newKeyServer(t, "key-1", bearerOf)
	_, err := (&http.Client{Transport: keyTransport{keys: f.keySource, set: bearer}.wrap(nil)}).Get(srv.URL)
	if !errors.Is(err, ErrKeyCommand) || len(srv.seen) != 0 {
		t.Fatalf("err = %v, requests %d", err, len(srv.seen))
	}
}

func TestHeaderSetters(t *testing.T) {
	h := http.Header{"Authorization": {"Bearer placeholder"}}
	xAPIKey(h, "k")
	if h.Get("X-Api-Key") != "k" || h.Get("Authorization") != "" {
		t.Errorf("x-api-key: %v", h)
	}
	h = http.Header{"X-Goog-Api-Key": {"placeholder"}}
	googAPIKey(h, "k")
	if h.Get("X-Goog-Api-Key") != "k" || len(h) != 1 {
		t.Errorf("x-goog-api-key: %v", h)
	}
	h = http.Header{}
	bearer(h, "k")
	if h.Get("Authorization") != "Bearer k" {
		t.Errorf("bearer: %v", h)
	}
}

// Each vendor's client, built as New builds it, sends the command's key
// where that vendor reads it, and replaces it when it is refused.
func TestClientsSendTheCommandsKey(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header func(*http.Request) string
		call   func(t *testing.T, keys *keySource, url string)
	}{
		{"openresponses", bearerOf, func(t *testing.T, keys *keySource, url string) {
			c := openresponses.NewClient(url, openresponses.WithMiddleware(func(next http.RoundTripper) http.RoundTripper {
				return transport(Spec{}, keys, bearer, next)
			}))
			_, _ = c.Create(context.Background(), openresponses.Request{Model: "m"})
		}},
		{"anthropic", func(r *http.Request) string {
			if r.Header.Get("Authorization") != "" {
				return "both headers"
			}
			return r.Header.Get("X-Api-Key")
		}, func(t *testing.T, keys *keySource, url string) {
			// The SDK's own variables must not add a credential.
			t.Setenv("ANTHROPIC_API_KEY", "from-env")
			t.Setenv("ANTHROPIC_AUTH_TOKEN", "from-env")
			opts := append(anthropicOptions("", keys, transport(Spec{}, keys, xAPIKey, nil)), option.WithBaseURL(url))
			c := sdk.NewClient(opts...)
			_, _ = c.Models.List(context.Background(), sdk.ModelListParams{})
		}},
		{"gemini", func(r *http.Request) string { return r.Header.Get("X-Goog-Api-Key") }, func(t *testing.T, keys *keySource, url string) {
			cc := geminiConfig("", keys, transport(Spec{}, keys, googAPIKey, nil))
			cc.HTTPOptions.BaseURL = url
			c, err := genai.NewClient(context.Background(), cc)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = c.Models.Get(context.Background(), "gemini-2.5-pro", nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newKeyServer(t, "key-2", tc.header)
			f := newFakeKeys()
			tc.call(t, f.keySource, srv.URL)
			if len(srv.seen) < 2 || srv.seen[0] != "key-1" || srv.seen[1] != "key-2" {
				t.Errorf("keys sent %v, want key-1 then key-2", srv.seen)
			}
		})
	}
}

// A model call that fails for want of a key, through each vendor's
// client as New builds it, is a *KeyError the turn loop does not retry,
// and a failing command is run once for all the clients' retries.
func TestModelCallsReportKeyErrors(t *testing.T) {
	type build func(t *testing.T, keys *keySource, url string) openresponses.Streamer
	clients := []struct {
		name   string
		header func(*http.Request) string
		build  build
	}{
		{"openresponses", bearerOf, func(t *testing.T, keys *keySource, url string) openresponses.Streamer {
			return openresponses.NewClient(url, openresponses.WithMiddleware(keyTransport{keys: keys, set: bearer}.wrap)).AsAdapter()
		}},
		{"anthropic", func(r *http.Request) string { return r.Header.Get("X-Api-Key") }, func(t *testing.T, keys *keySource, url string) openresponses.Streamer {
			c := sdk.NewClient(append(anthropicOptions("", keys, transport(Spec{}, keys, xAPIKey, nil)), option.WithBaseURL(url))...)
			return anthropic.New(c.Messages)
		}},
		{"gemini", func(r *http.Request) string { return r.Header.Get("X-Goog-Api-Key") }, func(t *testing.T, keys *keySource, url string) openresponses.Streamer {
			cc := geminiConfig("", keys, transport(Spec{}, keys, googAPIKey, nil))
			cc.HTTPOptions.BaseURL = url
			c, err := genai.NewClient(context.Background(), cc)
			if err != nil {
				t.Fatal(err)
			}
			return gemini.New(c)
		}},
	}
	for _, c := range clients {
		for _, status := range []int{401, 403} {
			t.Run(fmt.Sprintf("%s refused %d", c.name, status), func(t *testing.T) {
				srv := newKeyServer(t, "never", c.header)
				srv.refuse = status
				f := newFakeKeys()
				f.login = "my-token-helper login"
				m := withKeyErrors(c.build(t, f.keySource, srv.URL), f.keySource)
				err := m.CreateStream(context.Background(), openresponses.Request{Model: "m", Input: openresponses.Items{openresponses.UserText("hi")}}, discard)
				var ke *KeyError
				if !errors.As(err, &ke) || !errors.Is(err, ErrKeyRefused) || ke.Status != status {
					t.Fatalf("err = %v", err)
				}
				if !strings.HasPrefix(err.Error(), "authentication failed; sign in with my-token-helper login") || !strings.Contains(err.Error(), "unauthorized") {
					t.Errorf("message: %v", err)
				}
				if agentturn.DefaultRetryable(err) {
					t.Error("the turn loop would retry it")
				}
			})
		}
		t.Run(c.name+" command fails", func(t *testing.T) {
			srv := newKeyServer(t, "key-1", c.header)
			f := newFakeKeys()
			f.run = func(context.Context, []string) (string, error) {
				f.fails++
				return "", &KeyError{Program: "helper", Reason: "exit status 1", Detail: "not signed in"}
			}
			m := withKeyErrors(c.build(t, f.keySource, srv.URL), f.keySource)
			err := m.CreateStream(context.Background(), openresponses.Request{Model: "m", Input: openresponses.Items{openresponses.UserText("hi")}}, discard)
			if !errors.Is(err, ErrKeyCommand) || !strings.Contains(err.Error(), "not signed in") {
				t.Fatalf("err = %v", err)
			}
			if agentturn.DefaultRetryable(err) {
				t.Error("the turn loop would retry it")
			}
			if f.fails != 1 || len(srv.seen) != 0 {
				t.Errorf("command ran %d times, %d requests sent", f.fails, len(srv.seen))
			}
		})
	}
}

// A key the server refuses, replaced by a run that fails, is reported
// as the command's failure: what it says is the more use.
func TestRefusalThenFailedRunReportsTheCommand(t *testing.T) {
	srv := newKeyServer(t, "never", bearerOf)
	f := newFakeKeys()
	run := f.run
	f.run = func(ctx context.Context, argv []string) (string, error) {
		if f.runs.Load() > 0 {
			return "", &KeyError{Program: "helper", Reason: "exit status 1", Detail: "session expired"}
		}
		return run(ctx, argv)
	}
	m := withKeyErrors(openresponses.NewClient(srv.URL, openresponses.WithMiddleware(keyTransport{keys: f.keySource, set: bearer}.wrap)).AsAdapter(), f.keySource)
	err := m.CreateStream(context.Background(), openresponses.Request{Model: "m", Input: openresponses.Items{openresponses.UserText("hi")}}, discard)
	if !errors.Is(err, ErrKeyCommand) || !strings.Contains(err.Error(), "session expired") {
		t.Fatalf("err = %v", err)
	}
	if len(srv.seen) != 1 {
		t.Errorf("%d requests sent, want 1", len(srv.seen))
	}
}

var discard = openresponses.EventSinkFunc(func(openresponses.StreamEvent) error { return nil })

func TestWithKeyErrorsKeepsCompaction(t *testing.T) {
	keys := newFakeKeys().keySource
	if _, ok := withKeyErrors(recorder{}, keys).(compactor); ok {
		t.Error("a model that does not compact does")
	}
	if _, ok := withKeyErrors(openresponses.NewClient("http://x").AsAdapter(), keys).(compactor); !ok {
		t.Error("a model that compacts does not")
	}
}
