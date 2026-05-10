package domain

import "github.com/zereker/llm-gateway/pkg/repo"

// Endpoint / EndpointCapabilities / EndpointForm 都是 pkg/repo 同名类型的别名。
//
// 真实定义在 pkg/repo（带 sqlx + gorm tag、Scanner/Valuer），
// 这里保留 alias 让 middleware / adapter / schedule / ratelimit 等
// 现有代码不需要改 import。
type (
	Endpoint             = repo.Endpoint
	EndpointCapabilities = repo.EndpointCapabilities
	EndpointForm         = repo.EndpointForm
)

// 常量也别名一下。
const (
	FormVendor     = repo.FormVendor
	FormSelfHosted = repo.FormSelfHosted
)
