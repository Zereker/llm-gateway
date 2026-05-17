package domain

// UserIdentity M2 Auth middleware 的产物（凭证查表得到的主账号 + 子账户上下文）。
//
// **AccountID** 是主账号 pin / 计费主体；M5 用它判定模型订阅，
// M6 用它命中主账号级 quota policy。
//
// **QuotaPolicy 双层指针**：两层策略彼此独立、叠加生效；任一层超限都会拒绝。
// NULL = 该层不限。
//
// 设计原则（docs/06 §3）：纯业务结构，无 SQL tag、无 Scanner/Valuer、不 import repo。
type UserIdentity struct {
	AccountID            string
	SubAccountID         string
	APIKeyID             string
	Group                string
	ExternalUser         bool
	AccountQuotaPolicyID *int64
	APIKeyQuotaPolicyID  *int64
}

// Credentials 从请求头提取的鉴权凭证；IdentityProvider.Resolve 的入参。
type Credentials struct {
	APIKey      string            // "Authorization: Bearer xxx" 或 "X-API-Key: xxx" 提取
	BearerToken string            // JWT 形态时使用
	Headers     map[string]string // 完整透传
}
