package provider

import (
	"context"

	sdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/ChristopherDavenport/openresponses/providers/anthropic"
)

// cacheTTLs are the cache_ttl values and what each asks the adapter
// for. 5m is Claude's default, so it asks for nothing: a request sent
// with it is the one sent without a TTL.
var cacheTTLs = map[string]sdk.CacheControlEphemeralTTL{
	"5m": "",
	"1h": sdk.CacheControlEphemeralTTLTTL1h,
}

// claudeAdapters are the Claude adapters over messages for the main
// conversation and for the sub-agents, each caching for its own TTL,
// and whether they are two. When the two TTLs ask for the same, the
// sub-agents share the main adapter.
//
// The main conversation is where a 1-hour cache pays: its prefix is
// the whole session, and a user who comes back after twenty minutes
// reads it again. A sub-agent's prefix is its own short run, rarely
// read again after it ends, so writing it at the 1-hour price (twice
// the input price, against 1.25 times for five minutes) is mostly
// waste; that is why the two are set apart.
func claudeAdapters(messages sdk.MessageService, spec Spec) (main, sub openresponses.Adapter, separate bool) {
	adapter := func(ttl string) openresponses.Adapter {
		var opts []anthropic.Option
		if t := cacheTTLs[ttl]; t != "" {
			opts = append(opts, anthropic.WithCacheTTL(t))
		}
		return withClaudeCache(anthropic.New(messages, opts...))
	}
	main = adapter(spec.CacheTTL)
	if cacheTTLs[spec.SubagentCacheTTL] == cacheTTLs[spec.CacheTTL] {
		return main, main, false
	}
	return main, adapter(spec.SubagentCacheTTL), true
}

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
