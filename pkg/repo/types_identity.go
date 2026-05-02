package repo

// UserIdentity M2 Auth middleware 的产物（凭证查表得到的用户身份 + 租户上下文）。
//
// **TenantID** 是多租户的源头：M2 把它从 api_keys 表读出，后续所有 middleware
// （M5 ModelService / M7 Schedule 等）按 TenantID 限定查询范围。
// v0.1 单租户运行时所有记录都是 "default"。
type UserIdentity struct {
	TenantID     string // 租户 ID；v0.1 默认 "default"
	UserID       string // 平台内的用户唯一标识
	APIKeyID     string // 命中的 API Key 的 ID（用于审计与限流维度）
	Group        string // 限流 / 调度分组；默认 "default"，可扩展 "reserved" / "premium" 等
	ExternalUser bool   // true = 第三方付费用户（需走预算检查）；false = 内部 / 免费
}

// Credentials 从请求头提取的鉴权凭证；IdentityProvider.Resolve 的入参。
type Credentials struct {
	APIKey      string            // "Authorization: Bearer xxx" 或 "X-API-Key: xxx" 提取
	BearerToken string            // JWT 形态时使用
	Headers     map[string]string // 完整透传，自定义实现可用
}
