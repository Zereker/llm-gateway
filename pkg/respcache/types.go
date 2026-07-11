package respcache

import cacheport "github.com/zereker/llm-gateway/internal/cache"

// CachedResponse is one complete cached non-streaming response. The value is
// owned by the cache capability so concrete stores do not depend on HTTP
// middleware types.
type CachedResponse = cacheport.CachedResponse
