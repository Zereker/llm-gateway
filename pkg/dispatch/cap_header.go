package dispatch

import (
	"strconv"
	"strings"
)

// HeaderAttemptCap attempts 上限语义：
//
//	cfg.Default = cfg.Selector.MaxAttempts（默认 3）
//	客户端 X-Gateway-Max-Attempts header 仅允许往**更紧**（更小）的方向覆盖；
//	不能比 Default 更大（防恶意拉高网关 attempts 上限）。
//
// **header 解析**：middleware/schedule.go 在调 Dispatch 之前读
// c.GetHeader("X-Gateway-Max-Attempts")，把原始字符串塞进
// Input.AttemptCapOverride；本 Policy 解析 + clamp。
type HeaderAttemptCap struct {
	Default int // 全局默认；必须 > 0
}

// Resolve 计算本请求的 attempt cap。
//
//	override > 0 && override < Default → override
//	否则 → Default
func (h HeaderAttemptCap) Resolve(in Input) int {
	def := h.Default
	if def <= 0 {
		def = 3
	}
	raw := strings.TrimSpace(in.AttemptCapOverride)
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
