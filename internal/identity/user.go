// Package identity 定义鉴权后的用户身份模型。
//
// User 是 M2 Auth middleware 的产物，由 IdentityProvider 实现。
package identity

// User 鉴权后的用户身份。
type User struct {
	UserID       string // 平台内的用户唯一标识
	APIKeyID     string // 命中的 API Key 的 ID（用于审计与限流维度）
	Group        string // 限流 / 调度分组；默认 "default"，可扩展 "reserved" / "premium" 等
	ExternalUser bool   // true = 第三方付费用户（需走预算检查）；false = 内部 / 免费
}
