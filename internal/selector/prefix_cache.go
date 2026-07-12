package selector

import (
	"context"
	"hash/fnv"
	"sort"
	"strconv"

	"github.com/zereker/llm-gateway/internal/domain"
)

// PrefixCacheFilter routes requests with the same prefix to the same endpoint, improving
// KV-cache hit rate for self-hosted models (vLLM / SGLang / TensorRT-LLM etc).
//
// **Algorithm**: consistent hash ring + virtual nodes.
//   - Each candidate endpoint gets vnodes virtual nodes on the ring (default 64)
//   - FNV-64a hash of req.PrefixKey gives the ring position
//   - Find the first virtual node on the ring with hash >= ring position, return its endpoint
//
// **vnodes choice**: 64 is a common trade-off (even enough distribution without making the ring too large);
// with N=10 candidates the ring has 640 vnodes.
//
// **Degenerate behavior**:
//   - req.PrefixKey empty → pass through all candidates (let the selector later in the chain decide)
//   - candidates empty → empty
//
// **Must be placed last in the Filter chain** (selector semantics; returns 1). Pick either this
// or weighted_random — don't put both in the same chain.
//
// **Rebalance property**: when the candidate set changes (cooldown removes one ep), only about
// 1/N of prefixes get remapped; the rest still hit their original ep. This is the key benefit
// of consistent hashing — much better than simple hash%N, which remaps almost everything.
//
// Concurrency-safe (the ring is rebuilt on every Apply; no shared state. If candidates grows
// large this could be optimized with a per-request cache, but currently candidates number in
// the single-to-double digits, so rebuild cost is negligible).
type PrefixCacheFilter struct {
	vnodes int
}

// NewPrefixCacheFilter constructs a filter; uses default 64 when vnodes is 0.
func NewPrefixCacheFilter(vnodes int) *PrefixCacheFilter {
	if vnodes <= 0 {
		vnodes = 64
	}

	return &PrefixCacheFilter{vnodes: vnodes}
}

func (f *PrefixCacheFilter) Name() string { return "prefix_cache" }

// Apply implements Filter.Apply.
func (f *PrefixCacheFilter) Apply(_ context.Context, candidates []*domain.Endpoint, req *Request) []*domain.Endpoint {
	if len(candidates) == 0 {
		return nil
	}

	if req == nil || len(req.PrefixKey) == 0 {
		// prefix empty → pass through; let the selector later in the chain (weighted_random / etc) choose
		return candidates
	}

	type node struct {
		hash uint64
		ep   *domain.Endpoint
	}

	ring := make([]node, 0, len(candidates)*f.vnodes)
	for _, ep := range candidates {
		for v := 0; v < f.vnodes; v++ {
			h := fnv.New64a()
			_, _ = h.Write([]byte(strconv.FormatInt(ep.ID, 10)))
			_, _ = h.Write([]byte{'#'})
			_, _ = h.Write([]byte(strconv.Itoa(v)))
			ring = append(ring, node{hash: h.Sum64(), ep: ep})
		}
	}

	sort.Slice(ring, func(i, j int) bool { return ring[i].hash < ring[j].hash })

	keyHash := fnv.New64a()
	_, _ = keyHash.Write(req.PrefixKey)
	target := keyHash.Sum64()

	idx := sort.Search(len(ring), func(i int) bool { return ring[i].hash >= target })
	if idx == len(ring) {
		idx = 0 // wrap
	}

	return []*domain.Endpoint{ring[idx].ep}
}

// Compile-time assertion.
var _ Filter = (*PrefixCacheFilter)(nil)
