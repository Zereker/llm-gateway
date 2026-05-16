package repo

// UserIdentity M2 Auth middleware 的产物（凭证查表得到的主账号 + 子账户上下文）。
//
// **AccountID** 是主账号 pin / 计费主体，不是“用户”。M2 从 api_keys 表读出它，M5 用它判定模型订阅，
// M6 用它命中主账号级 quota policy，M5 用它获取主账号对应的 pricing version。
//
// **SubAccountID** 是主账号下的子账户 / 操作者，用于审计、usage meta 和后续按
// 子账户统计；当前不作为 M6 限流主维度。
//
// **QuotaPolicy 双层指针**：M2 一次 SELECT JOIN accounts 把“主账号级”和
// “API key 级”两个 quota_policy_id 都拿出来。两层策略彼此独立、叠加生效；
// 任一层超限都会拒绝。NULL = 该层不限。
type UserIdentity struct {
	AccountID            string // 主账号 pin / 计费主体；v0.1 默认 "default"
	SubAccountID         string // 子账户 / 操作者标识
	APIKeyID             string // 命中的 API Key 的稳定 ID（审计与限流维度）
	Group                string // 限流 / 调度分组；默认 "default"
	ExternalUser         bool   // true = 第三方付费用户（需走预算检查）
	AccountQuotaPolicyID *int64 // 主账号级限流策略 ID；nil = 主账号层不限
	APIKeyQuotaPolicyID  *int64 // API key 级限流策略 ID；nil = key 层不限
}

// Credentials 从请求头提取的鉴权凭证；IdentityProvider.Resolve 的入参。
type Credentials struct {
	APIKey      string            // "Authorization: Bearer xxx" 或 "X-API-Key: xxx" 提取
	BearerToken string            // JWT 形态时使用
	Headers     map[string]string // 完整透传，自定义实现可用
}
