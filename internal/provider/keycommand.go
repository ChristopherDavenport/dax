package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/ChristopherDavenport/openresponses"
)

// KeyTTL is how long a key a command printed is used before the
// command is run again. It is short enough that a credential which
// expires within the hour is replaced well before it does, and long
// enough that the command is not run on every request.
const KeyTTL = 5 * time.Minute

// keyCommandTimeout bounds one run of the key command. A command may
// wait on an interactive sign-in in a browser, so it gets minutes, not
// seconds; a caller that gives up sooner (the user aborting the turn)
// stops waiting at once.
const keyCommandTimeout = 5 * time.Minute

// keyFailHold is how long a failed run is the answer to every caller
// before the command is run again, so that the retries of one request,
// the SDK's and the agent's, do not run it again each.
const keyFailHold = 5 * time.Second

// maxKey is the longest output taken as a key.
const maxKey = 16 << 10

// stderrTail is how much of the command's standard error is kept for
// the error that reports it.
const stderrTail = 4 << 10

var (
	// ErrKeyCommand is the error for a key command that failed or
	// printed no usable key.
	ErrKeyCommand = errors.New("api key command")
	// ErrKeyRefused is the error for a request the server refused with
	// 401 or 403 even with a freshly fetched key.
	ErrKeyRefused = errors.New("api key refused")
)

// KeyError says why there is no key the server accepts, and how to get
// one when the user has said (Login). It is ErrKeyCommand when the
// command failed and ErrKeyRefused when the server refused its key,
// and nothing else: what the client made of the failure, a 500 or a
// network error, is not worth a retry. It never carries the key.
type KeyError struct {
	// Program is the command's program, without its arguments.
	Program string
	// Reason is what went wrong with the run: its exit status, a
	// timeout, or what is wrong with what it printed.
	Reason string
	// Detail is the last line the command wrote to standard error, or
	// for a refusal what the server said.
	Detail string
	// Status is the server's 401 or 403; zero when the command failed.
	Status int
	// Login is what the user configured to sign in again; may be empty.
	Login string
}

func (e *KeyError) Error() string {
	var b strings.Builder
	// What to do comes first: a status line shows only what fits.
	b.WriteString("authentication failed")
	if e.Login != "" {
		b.WriteString("; sign in with " + e.Login + ", then try again")
	}
	b.WriteString(" (")
	if e.Status != 0 {
		fmt.Fprintf(&b, "the server refused the key from %s with %d", e.Program, e.Status)
	} else {
		fmt.Fprintf(&b, "api key command %s: %s", e.Program, e.Reason)
	}
	if e.Detail != "" {
		b.WriteString(": " + e.Detail)
	}
	b.WriteString(")")
	if e.Login == "" {
		b.WriteString("; this usually means signing in to the provider again")
	}
	return b.String()
}

func (e *KeyError) Is(target error) bool {
	if e.Status != 0 {
		return target == ErrKeyRefused
	}
	return target == ErrKeyCommand
}

// keySource hands out the key a command prints, caching it for ttl.
// Callers that need a key at the same time share one run of the
// command, and a caller whose key was refused asks for another with
// Refresh. The key is never logged, put in an error or set in an
// environment.
type keySource struct {
	argv  []string
	login string
	ttl   time.Duration
	now   func() time.Time
	run   func(ctx context.Context, argv []string) (string, error)
	// echo is where the command's standard error goes as it is written,
	// besides being kept for the error; nil keeps it only.
	echo atomic.Pointer[io.Writer]

	mu      sync.Mutex
	key     string
	fetched time.Time
	failed  time.Time
	failErr error
	pending *fetch // the run in progress; nil when there is none
}

// fetch is one run of the command; done is closed when key and err
// are set.
type fetch struct {
	done chan struct{}
	key  string
	err  error
}

func newKeySource(argv []string, login string) *keySource {
	s := &keySource{argv: argv, login: login, ttl: KeyTTL, now: time.Now}
	var w io.Writer = os.Stderr
	s.echo.Store(&w)
	s.run = func(ctx context.Context, argv []string) (string, error) {
		return runKeyCommand(ctx, argv, *s.echo.Load())
	}
	return s
}

// setEcho sends the command's standard error to w as it is written; nil
// keeps it only for the error, for a front that owns the screen.
func (s *keySource) setEcho(w io.Writer) { s.echo.Store(&w) }

// Key returns the cached key, running the command when there is none
// or it is older than the TTL.
func (s *keySource) Key(ctx context.Context) (string, error) { return s.get(ctx, "") }

// Refresh returns a key to use in place of stale, the one the server
// refused. If another caller has already replaced stale, that key is
// returned without running the command again. The result can be stale
// itself when the command prints the same key.
func (s *keySource) Refresh(ctx context.Context, stale string) (string, error) {
	return s.get(ctx, stale)
}

func (s *keySource) get(ctx context.Context, stale string) (string, error) {
	s.mu.Lock()
	if s.key != "" && s.key != stale && s.now().Sub(s.fetched) < s.ttl {
		key := s.key
		s.mu.Unlock()
		return key, nil
	}
	if s.failErr != nil && s.now().Sub(s.failed) < keyFailHold {
		err := s.failErr
		s.mu.Unlock()
		return "", err
	}
	f := s.pending
	if f == nil {
		f = &fetch{done: make(chan struct{})}
		s.pending = f
		// The run is shared, so one caller giving up must not cut it
		// short for the others; keyCommandTimeout bounds it instead.
		go s.fill(f)
	}
	s.mu.Unlock()
	select {
	case <-f.done:
		return f.key, f.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (s *keySource) fill(f *fetch) {
	f.key, f.err = s.run(context.Background(), s.argv)
	var ke *KeyError
	if errors.As(f.err, &ke) {
		ke.Login = s.login
	}
	s.mu.Lock()
	if f.err == nil {
		s.key, s.fetched, s.failErr = f.key, s.now(), nil
	} else {
		s.failed, s.failErr = s.now(), f.err
	}
	s.pending = nil
	s.mu.Unlock()
	close(f.done)
}

// runKeyCommand runs argv without a shell, standard input closed, and
// returns its trimmed standard output, which must be one token of
// printable ASCII. Standard error is kept for the error and also
// written to echo, when it is not nil, so a command that prompts or
// explains itself is seen.
func runKeyCommand(ctx context.Context, argv []string, echo io.Writer) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, keyCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var out bytes.Buffer
	errTail := &tail{max: stderrTail}
	cmd.Stdout = &out
	cmd.Stderr = errTail
	if echo != nil {
		cmd.Stderr = io.MultiWriter(errTail, echo)
	}
	fail := func(reason string) error {
		return &KeyError{Program: argv[0], Reason: reason, Detail: errTail.lastLine()}
	}
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fail(fmt.Sprintf("no key within %v", keyCommandTimeout))
		}
		return "", fail(err.Error())
	}
	key := strings.TrimSpace(out.String())
	if key == "" {
		return "", fail("printed no key")
	}
	if len(key) > maxKey {
		return "", fail(fmt.Sprintf("printed more than %d bytes", maxKey))
	}
	if strings.ContainsAny(key, "\r\n") {
		return "", fail("printed more than one line")
	}
	for _, r := range key {
		if r <= ' ' || r > '~' {
			return "", fail("printed something besides the key (a space or a character a header cannot carry)")
		}
	}
	return key, nil
}

// tail keeps the last max bytes written to it.
type tail struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = t.buf[over:]
	}
	return len(p), nil
}

// lastLine is the last non-blank line written, as oneLine has it.
func (t *tail) lastLine() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := strings.Split(string(t.buf), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := oneLine(lines[i]); l != "" {
			return l
		}
	}
	return ""
}

// oneLine is s without what would act on a terminal, cut to a length
// an error line can carry.
func oneLine(s string) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return ' '
		case r < ' ' || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == utf8.RuneError:
			return -1
		}
		return r
	}, s))
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "…"
	}
	return s
}

// keyTransport puts the source's key on every request with set. When
// the server answers 401 or 403 it asks for a fresh key and sends the
// request once more, provided the body can be read again; otherwise
// the response is returned as it came. A refusal that stands is noted
// on the request's context for keyStreamer, and so is a key the
// command could not give.
type keyTransport struct {
	keys *keySource
	set  func(h http.Header, key string)
	next http.RoundTripper
}

func refused(res *http.Response) bool {
	return res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden
}

func (t *keyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	note, _ := req.Context().Value(authNoteKey{}).(*authNote)
	key, err := t.keys.Key(req.Context())
	if err != nil {
		note.fail(err)
		return nil, err
	}
	res, err := t.next.RoundTrip(t.with(req, key))
	if err != nil || !refused(res) {
		return res, err
	}
	res = t.retry(req, res, key, note)
	if refused(res) && note != nil {
		note.status.Store(int32(res.StatusCode))
	}
	return res, nil
}

// retry sends req again with a fresh key in place of key, which the
// server refused with res; it returns res when it cannot. A command
// that fails to give one is noted, as it says more than the refusal.
func (t *keyTransport) retry(req *http.Request, res *http.Response, key string, note *authNote) *http.Response {
	if req.Body != nil && req.GetBody == nil {
		return res
	}
	fresh, err := t.keys.Refresh(req.Context(), key)
	if err != nil {
		note.fail(err)
		return res
	}
	if fresh == key {
		return res
	}
	again := t.with(req, fresh)
	if req.GetBody != nil {
		if again.Body, err = req.GetBody(); err != nil {
			return res
		}
	}
	next, err := t.next.RoundTrip(again)
	if err != nil {
		return res
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	_ = res.Body.Close()
	return next
}

// with is a copy of req carrying key: a RoundTripper must not change
// the request it was given.
func (t *keyTransport) with(req *http.Request, key string) *http.Request {
	r := req.Clone(req.Context())
	t.set(r.Header, key)
	return r
}

// wrap is t over next, for clients that take a transport middleware.
func (t keyTransport) wrap(next http.RoundTripper) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	t.next = next
	return &t
}

// bearer, xAPIKey and googAPIKey put a key where each vendor reads it.
func bearer(h http.Header, key string) { h.Set("Authorization", "Bearer "+key) }

func xAPIKey(h http.Header, key string) {
	h.Del("Authorization")
	h.Set("X-Api-Key", key)
}

func googAPIKey(h http.Header, key string) { h.Set("X-Goog-Api-Key", key) }

// authNote is where keyTransport records, for one model call, a 401 or
// 403 that a fresh key did not cure, or the command's failure: the
// vendors' adapters report a transport error as text in an error of
// their own, so it cannot be found by unwrapping.
type authNote struct {
	status atomic.Int32
	failed atomic.Pointer[KeyError]
}

// fail notes err when it is the command's failure; n may be nil.
func (n *authNote) fail(err error) {
	var ke *KeyError
	if n != nil && errors.As(err, &ke) {
		n.failed.Store(ke)
	}
}

type authNoteKey struct{}

// keyStreamer reports a model call that failed for want of a key as a
// *KeyError, whichever vendor's client made it: the command's failure
// or the refusal keyTransport noted, in place of what the client made
// of it.
type keyStreamer struct {
	inner openresponses.Streamer
	keys  *keySource
}

func (s *keyStreamer) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	note := &authNote{}
	err := s.inner.CreateStream(context.WithValue(ctx, authNoteKey{}, note), req, sink)
	return s.explain(err, note)
}

func (s *keyStreamer) explain(err error, note *authNote) error {
	if err == nil {
		return nil
	}
	var ke *KeyError
	if errors.As(err, &ke) {
		return ke
	}
	if ke := note.failed.Load(); ke != nil {
		return ke
	}
	if status := int(note.status.Load()); status != 0 {
		return &KeyError{Program: s.keys.argv[0], Status: status, Detail: oneLine(err.Error()), Login: s.keys.login}
	}
	return err
}

// compactingKeyStreamer is keyStreamer over a model that compacts, so
// that whoever asks whether the model compacts gets the same answer.
type compactingKeyStreamer struct {
	*keyStreamer
	c compactor
}

type compactor interface {
	Compact(context.Context, openresponses.CompactRequest) (*openresponses.CompactResponse, error)
}

func (s *compactingKeyStreamer) Compact(ctx context.Context, req openresponses.CompactRequest) (*openresponses.CompactResponse, error) {
	note := &authNote{}
	res, err := s.c.Compact(context.WithValue(ctx, authNoteKey{}, note), req)
	return res, s.explain(err, note)
}

// withKeyErrors wraps s in keyStreamer.
func withKeyErrors(s openresponses.Streamer, keys *keySource) openresponses.Streamer {
	k := &keyStreamer{inner: s, keys: keys}
	if c, ok := s.(compactor); ok {
		return &compactingKeyStreamer{keyStreamer: k, c: c}
	}
	return k
}
