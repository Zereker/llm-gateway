package schedule

import (
	"context"
	"hash/fnv"
	"sort"
	"strconv"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// PrefixCacheFilter 把同 prefix 的请求路由到同一个 endpoint，让 self-hosted 模型（vLLM /
// SGLang / TensorRT-LLM 等）的 KV-cache 命中。
//
// **算法**：一致性哈希环 + 虚拟节点。
//   - 每个 candidate endpoint 在环上挂 vnodes 个虚拟节点（默认 64）
//   - 对 req.PrefixKey 做 FNV-64a 哈希得到 ring position
//   - 找环上第一个 hash >= ring position 的虚拟节点，返回其 endpoint
//
// **vnodes 选择**：64 是常见 trade-off（够分布均匀，又不至于 ring 太大）；
// 候选数 N=10 时环上有 640 个 vnode。
//
// **退化行为**：
//   - req.PrefixKey 空 → 透传所有 candidates（让 chain 后面的 selector 决定）
//   - candidates 空 → 空
//
// **必须放 Filter 链最后一个**（selector 语义；返回 1 个）。跟 weighted_random
// 二选一——同一条 chain 里不要同时挂。
//
// **rebalance 性质**：candidates 集合变（被 cooldown 移除一个 ep）时，只有大约
// 1/N 的 prefix 会被重映射；其余仍命中原 ep。这是 consistent hashing 的关键
// 收益——比简单 hash%N 重映射几乎全部要好得多。
//
// 并发安全（每次 Apply 重建 ring；无共享状态。如果 candidates 大可后续优化做
// per-request cache，但当前 candidates 个位数到几十个，重建成本忽略不计）。
type PrefixCacheFilter struct {
	vnodes int
}

// NewPrefixCacheFilter 构造 filter；vnodes 0 时用默认 64。
func NewPrefixCacheFilter(vnodes int) *PrefixCacheFilter {
	if vnodes <= 0 {
		vnodes = 64
	}
	return &PrefixCacheFilter{vnodes: vnodes}
}

func (f *PrefixCacheFilter) Name() string { return "prefix_cache" }

// Apply 实现 Filter.Apply。
func (f *PrefixCacheFilter) Apply(_ context.Context, candidates []*domain.Endpoint, req *Request) []*domain.Endpoint {
	if len(candidates) == 0 {
		return nil
	}
	if req == nil || len(req.PrefixKey) == 0 {
		// prefix 空 → 透传；让链后面的 selector（weighted_random / etc）选
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

// 编译期断言。
var _ Filter = (*PrefixCacheFilter)(nil)
