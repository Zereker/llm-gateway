package repo

import "github.com/zereker/llm-gateway/pkg/domain"

// 类型迁移到 pkg/domain（docs/06 §3：domain 是业务结构真相，repo 只提供 SQL 实现）。
// 保留 type alias 是为了让 SQLAPIKeyProvider.Resolve 等内部代码继续返回原名。
//
// **新代码用 domain.UserIdentity / domain.Credentials**；本 alias 仅为内部过渡。
type (
	UserIdentity = domain.UserIdentity
	Credentials  = domain.Credentials
)
