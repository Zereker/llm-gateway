package domain

import "github.com/zereker-labs/ai-gateway/pkg/repo"

// ModelServiceSnapshot 是 repo.ModelService 的别名（真实定义在 pkg/repo）。
//
// 老名 "Snapshot" 暗示"配置快照"语义；新代码可直接用 repo.ModelService。
type ModelServiceSnapshot = repo.ModelService
