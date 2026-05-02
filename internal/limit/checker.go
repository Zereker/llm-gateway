package limit

import (
	"context"

	"github.com/zereker-labs/ai-gateway/internal/identity"
	"github.com/zereker-labs/ai-gateway/internal/modelservice"
	"github.com/zereker-labs/ai-gateway/internal/usage"
)

// Checker M6 Limit middleware 的依赖接口。
type Checker interface {
	// BuildSpec 为本次请求构建用户层 + 模型层阈值（不含 Endpoint 层）。
	BuildSpec(id identity.User, ms *modelservice.Snapshot) *Spec

	// CheckReadOnly 三层预检（用户层 + 模型层），read-only。
	// Endpoint 层不在此处检查（在调度层 LimitReadFilter 内做）。
	CheckReadOnly(ctx context.Context, spec *Spec, id identity.User, ms *modelservice.Snapshot) CheckResult

	// PeekEndpoint 给调度层 LimitReadFilter 用：读 endpoint 层当前使用率。
	PeekEndpoint(ctx context.Context, endpointID string) EndpointUsage

	// Consume 响应成功后由 M10 Tracing 调用。按真实 Usage 扣三层桶。
	Consume(ctx context.Context, spec *Spec, id identity.User, u *usage.Usage) error
}

// CheckResult 三层预检的结果。
type CheckResult struct {
	UserBlocked    bool   // 用户层超限 → M6 直接 abort 429
	ServiceBlocked bool   // 模型层超限 → 不 abort，写 rc.Extras["service_blocked"]
	Reason         string
}

// EndpointUsage Endpoint 层当前使用情况。
type EndpointUsage struct {
	RPMUsed int64
	RPMCap  int64
	TPMUsed int64
	TPMCap  int64
	RPSUsed int64
	RPSCap  int64
}
