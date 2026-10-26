package provider

import (
	"context"

	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
)

// noSessionCacheKey is the prompt cache key of a Claude request made
// outside a run, whose context names no session. The anthropic adapter
// reads a key only as the request asking to be cached and sends Claude
// no value, so a fixed key opts such a request in as well as an ID
// would; leaving it without one would send it uncached.
const noSessionCacheKey = "dax"

// claudeCache sends every request to a Claude adapter with a prompt
// cache key, which is how that adapter opts a request into Claude's
// automatic prompt caching (5 minutes, refreshed by each hit): without
// one, every turn resends the whole conversation at the full input
// price. The key is the session the request's run is written to, from
// its context, so a sub-agent's requests carry the sub-agent's session
// as the session header does; a request that already has a key keeps
// it. Only Claude gets this: the gemini adapter refuses the field.
type claudeCache struct {
	openresponses.Adapter
}

// withClaudeCache wraps a Claude adapter in claudeCache.
func withClaudeCache(a openresponses.Adapter) openresponses.Adapter {
	return claudeCache{Adapter: a}
}

func (c claudeCache) Create(ctx context.Context, req openresponses.Request) (*openresponses.Response, error) {
	return c.Adapter.Create(ctx, withCacheKey(ctx, req))
}

func (c claudeCache) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	return c.Adapter.CreateStream(ctx, withCacheKey(ctx, req), sink)
}

// withCacheKey is req with a prompt cache key: its own, else the
// context's session ID, else noSessionCacheKey.
func withCacheKey(ctx context.Context, req openresponses.Request) openresponses.Request {
	if req.PromptCacheKey != "" {
		return req
	}
	req.PromptCacheKey = session.SessionIDFromContext(ctx)
	if req.PromptCacheKey == "" {
		req.PromptCacheKey = noSessionCacheKey
	}
	return req
}
