package agent

import (
	"context"
	"sync"

	"github.com/ChristopherDavenport/agentturn"
)

// truncationWatch warns, once a session, when the server counts fewer
// input tokens than for the call before while the request grew: a
// server past the window it runs the model at (Ollama's num_ctx) drops
// the oldest messages and answers as if nothing happened, and this is
// the one sign of it the client can see. A fold shrinks the request, so
// the first call after one is not compared.
type truncationWatch struct {
	log func(format string, args ...any)

	mu             sync.Mutex
	items, pending int // the last answered request's items, and the one in flight's
	tokens         int
	warned         bool
}

func (w *truncationWatch) handle(_ context.Context, ev agentturn.Event) error {
	switch e := ev.(type) {
	case *agentturn.TurnStart:
		w.mu.Lock()
		w.pending = len(e.Request.Input)
		w.mu.Unlock()
	case *agentturn.ResponseEnd:
		if e.Response == nil || e.Response.Usage == nil {
			return nil
		}
		in := e.Response.Usage.InputTokens
		w.mu.Lock()
		defer w.mu.Unlock()
		if !w.warned && w.tokens > 0 && w.pending > w.items && in < w.tokens {
			w.warned = true
			w.log("[the server counted fewer input tokens (%d) than for the call before (%d) while the conversation grew: it may be dropping the oldest messages past the window it runs the model at; set context_window or -compact below that window]", in, w.tokens)
		}
		w.items, w.tokens = w.pending, in
	}
	return nil
}
