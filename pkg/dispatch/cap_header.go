package dispatch

import (
	"strconv"
	"strings"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// HeaderAttemptCap 现状（pkg/middleware/selector.go）的 attempts 上限语义：
//
//	cfg.Default = cfg.Selector.MaxAttempts（默认 3）
//	header X-Gateway-Max-Attempts 仅允许往**更紧**（更小）的方向覆盖；
//	不能比 Default 更大（防恶意拉高网关 attempts 上限）。
//
// **header 解析**：从 rc.Extras["headers"] 读（middleware 在前置阶段写入）。
// 不在的话只用 Default。
type HeaderAttemptCap struct {
	Default int    // 全局默认；必须 > 0
	Header  string // header 名，默认 "X-Gateway-Max-Attempts"
}

// HeaderKey HeaderAttemptCap 从 rc.Extras 读 header 的 key。
//
// 装配点（M7 thin adapter）负责把 c.GetHeader(...) 的结果放到 rc.Extras
// 这个 key 下，AttemptCap 才能读到。
const HeaderKey = "_dispatch.header.max_attempts"

// Resolve 计算本请求的 attempt cap。
//
//	override > 0 && override < Default → override
//	否则 → Default
func (h HeaderAttemptCap) Resolve(rc *domain.RequestContext) int {
	def := h.Default
	if def <= 0 {
		def = 3
	}
	if rc == nil {
		return def
	}
	v, ok := rc.Extras[HeaderKey]
	if !ok {
		return def
	}
	raw, ok := v.(string)
	if !ok {
		return def
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	if n < def {
		return n
	}
	return def
}
