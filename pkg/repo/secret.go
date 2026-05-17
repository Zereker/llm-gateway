package repo

import "github.com/zereker/llm-gateway/pkg/domain"

// Secret 已迁到 pkg/domain（docs/06 §3）。保留 alias 让 sqlx 列扫描透明。
type Secret = domain.Secret
