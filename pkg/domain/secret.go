package domain

import "github.com/zereker-labs/ai-gateway/pkg/repo"

// Secret 是 repo.Secret 的别名（真实定义在 pkg/repo）。
//
// 保留本别名是为了不打乱现有 middleware / adapter 代码的 import；
// 新代码可直接用 repo.Secret。
type Secret = repo.Secret
