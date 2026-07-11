package repo

import (
	"context"
	"fmt"
	"time"

	lru "github.com/hashicorp/golang-lru/v2/expirable"
	"golang.org/x/sync/singleflight"
)

// TTLCache is an LRU with TTL + singleflight loader — at capacity it evicts
// least-recently-used; a Get past TTL counts as a miss; GetOrLoad folds
// "miss -> call loader -> backfill" into one step, and concurrent misses on
// the same key call the loader only once (thundering-herd protection).
//
// Underneath it's a thin wrapper around hashicorp/golang-lru v2 expirable.LRU.
//
// **Usage pattern**: the repo layer's cached wrapper uses GetOrLoad to wrap a SQL Reader.
//
//	c := repo.NewTTLCache[int64, *Endpoint](1024, 30*time.Second)
//	ep, err := c.GetOrLoad(ctx, id, func(ctx context.Context) (*Endpoint, bool, error) {
//	    v, err := sqlReader.GetByID(ctx, id)
//	    if err != nil { return nil, false, err }
//	    return v, v != nil, nil   // not-found (v==nil) is not cached
//	})
//
// **Design trade-off**: the repo side uses a TTL cache instead of real-time
// invalidation. After the deployer's SQL write completes, the gateway is only
// guaranteed to see the new value within <= TTL; we accept this delay because
// changes to business tables (endpoint / api_key enablement / quota
// adjustments) don't need sub-second propagation.
type TTLCache[K comparable, V any] struct {
	inner   *lru.LRU[K, V]
	sf      singleflight.Group
	metrics Metrics // optional; nil -> no reporting
	table   string  // table label used when reporting metrics
}

// LoaderFunc is the miss callback for GetOrLoad.
//
// Returns (value, cache, err):
//   - err != nil: not cached, the error is passed through to the caller
//   - err == nil && cache == true: value is backfilled into the cache and returned
//   - err == nil && cache == false: value is returned directly, not cached
//     (used for not-found: avoids caching a "just-created resource" that
//     hasn't taken effect yet)
type LoaderFunc[V any] func(ctx context.Context) (V, bool, error)

// NewTTLCache builds a TTLCache. Falls back to 1024 when capacity<=0.
func NewTTLCache[K comparable, V any](capacity int, ttl time.Duration) *TTLCache[K, V] {
	if capacity <= 0 {
		capacity = 1024
	}
	return &TTLCache[K, V]{inner: lru.NewLRU[K, V](capacity, nil, ttl)}
}

// WithMetrics attaches a Metrics + table label to the cache. Chainable call:
//
//	c := repo.NewTTLCache[int64, *Endpoint](1024, ttl).WithMetrics("endpoints", m)
//
// nil metrics is equivalent to not setting it (used when the cached wrapper
// doesn't pass metrics).
func (c *TTLCache[K, V]) WithMetrics(table string, m Metrics) *TTLCache[K, V] {
	c.metrics = m
	c.table = table
	return c
}

func (c *TTLCache[K, V]) Get(key K) (V, bool) { return c.inner.Get(key) }
func (c *TTLCache[K, V]) Set(key K, val V)    { c.inner.Add(key, val) }
func (c *TTLCache[K, V]) Delete(key K)        { c.inner.Remove(key) }
func (c *TTLCache[K, V]) Len() int            { return c.inner.Len() }

// loaderTimeout is the hard cap for a loader (one SQL point lookup / short list query).
//
// Because the loader runs on a ctx that is **decoupled from the leader
// request** (see GetOrLoad), it must carry its own deadline — otherwise, if
// the DB hangs, all singleflight waiters would block forever.
const loaderTimeout = 5 * time.Second

// GetOrLoad returns immediately on a cache hit; on a miss it uses singleflight
// to call the loader.
//
// Concurrent misses on the same key call the loader only once; the other
// callers block waiting for the same result. A loader panic / err is passed
// through to all blocked callers.
//
// **loader ctx is decoupled from the leader**: the singleflight result is
// shared by all waiters, but the loader closure only ever gets the ctx of the
// "first goroutine to arrive" — if we used it directly, that leader's client
// disconnecting (ctx cancel) would make the SQL fail with context.Canceled,
// **poisoning all waiters** (N innocent requests failing together). So the
// loader runs on context.WithoutCancel(ctx): trace / baggage values are kept,
// cancellation and deadline are stripped, and loaderTimeout is layered on top
// as a backstop. The cost is that the SQL query for a disconnected request
// still runs to completion — which conveniently backfills the cache, which is
// what we want.
//
// Reports Metrics with three outcomes: hit / miss / error (hit on a cache hit;
// miss when the loader succeeds; error when the loader returns an err). When
// multiple goroutines miss on the same key concurrently, only one miss is
// reported (singleflight runs the loader exactly once).
func (c *TTLCache[K, V]) GetOrLoad(ctx context.Context, key K, loader LoaderFunc[V]) (V, error) {
	if v, ok := c.inner.Get(key); ok {
		c.record("hit")
		return v, nil
	}
	// The singleflight key is a string; fmt.Sprintf on the comparable is fine
	// (the miss path isn't a hot path).
	sfKey := fmt.Sprintf("%v", key)
	raw, err, _ := c.sf.Do(sfKey, func() (any, error) {
		// By the time the block ends another goroutine may already have
		// backfilled it, so check again.
		if v, ok := c.inner.Get(key); ok {
			return v, nil
		}
		loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), loaderTimeout)
		defer cancel()
		v, cache, err := loader(loadCtx)
		if err != nil {
			c.record("error")
			return v, err
		}
		if cache {
			c.inner.Add(key, v)
		}
		c.record("miss")
		return v, nil
	})
	if err != nil {
		var zero V
		return zero, err
	}
	return raw.(V), nil
}

func (c *TTLCache[K, V]) record(result string) {
	if c.metrics != nil {
		c.metrics.Record(c.table, result)
	}
}
