package executor

import (
	"net/http"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// SessionIdle is how long a session served over HTTP may start no
// request before Handler closes it and ends its processes: a client
// that went away without saying so leaves nothing running for long. A
// var for the tests.
var SessionIdle = 5 * time.Minute

// KeepAlive is how often a client over HTTP pings the executor, well
// within SessionIdle, so a session idle while its user thinks stays
// open.
const KeepAlive = time.Minute

// sessionIDHeader is the streamable HTTP transport's session header.
const sessionIDHeader = "Mcp-Session-Id"

// Handler serves s, NewServer's, over MCP's streamable HTTP transport,
// for `dax execute -listen`, until done is closed. Each client that
// initializes is a session of its own, on an MCP server of its own
// (Server), with its own tools, built at the read limit it asked for,
// and its own processes (process.go keeps them by session and ends
// them with it); every session shares the workspace, and so the lock
// its writes take.
//
// A session ends when its client deletes it, or when it has started no
// request for SessionIdle. Either way its processes end first: a read
// or a wait on one is a request in flight, which a session's close
// waits for and which nothing over HTTP cancels when the client goes.
// The SDK's own idle timeout is not used, since its close would wait
// on such a request for ever, and it does not count a request that
// waits as idle. Its tools are closed once it has ended.
//
// It authenticates no one: whoever reaches it runs the tools. What it
// refuses is a browser's way in: a request that came to a loopback
// address naming another host (the SDK's DNS-rebinding check, left
// on), and a cross-origin request from a page.
func Handler(s *Server, done <-chan struct{}) http.Handler {
	h := &httpSessions{s: s, seen: map[string]time.Time{}}
	// The SDK asks for a server on every request, but serves one only
	// to a request that starts a session; a session's requests go to
	// the server it started on. probe answers the others, which read
	// only the protocol versions it takes.
	probe, err := s.session()
	h.next = sdk.NewStreamableHTTPHandler(func(r *http.Request) *sdk.Server {
		if r.Header.Get(sessionIDHeader) != "" {
			return probe
		}
		srv, err := s.session()
		if err != nil {
			return nil
		}
		return srv
	}, nil)
	if err != nil {
		// Closed already: serve nothing.
		h.next = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		})
	}
	go h.sweep(done)
	return http.NewCrossOriginProtection().Handler(h)
}

// httpSessions is Handler: the SDK's handler, with when each session
// last started a request.
type httpSessions struct {
	s    *Server
	next http.Handler

	mu   sync.Mutex
	seen map[string]time.Time // by session ID
}

func (h *httpSessions) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if id := r.Header.Get(sessionIDHeader); id != "" {
		h.mu.Lock()
		h.seen[id] = time.Now()
		h.mu.Unlock()
		if r.Method == http.MethodDelete {
			if ss := h.session(id); ss != nil {
				h.s.endSession(ss)
			}
		}
	}
	h.next.ServeHTTP(w, r)
}

// session is the session of id, nil if there is none.
func (h *httpSessions) session(id string) *sdk.ServerSession {
	for _, ss := range h.s.Sessions() {
		if ss.ID() == id {
			return ss
		}
	}
	return nil
}

// sweep closes, until done, each session that has started no request
// for SessionIdle, its processes first. A session is first seen when
// sweep finds it, if it has started no request since it initialized.
func (h *httpSessions) sweep(done <-chan struct{}) {
	tick := time.NewTicker(SessionIdle / 4)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-tick.C:
		}
		now := time.Now()
		live := map[string]bool{}
		var idle []*sdk.ServerSession
		h.mu.Lock()
		for _, ss := range h.s.Sessions() {
			id := ss.ID()
			if id == "" {
				continue
			}
			live[id] = true
			last, ok := h.seen[id]
			if !ok {
				h.seen[id] = now
			} else if now.Sub(last) >= SessionIdle {
				idle = append(idle, ss)
				delete(h.seen, id)
			}
		}
		for id := range h.seen {
			if !live[id] {
				delete(h.seen, id)
			}
		}
		h.mu.Unlock()
		for _, ss := range idle {
			h.s.endSession(ss)
			go ss.Close()
		}
	}
}

// HTTPTransport is the client's transport to an executor Handler serves
// at endpoint, an http or https URL.
func HTTPTransport(endpoint string) sdk.Transport {
	return &sdk.StreamableClientTransport{Endpoint: endpoint}
}
