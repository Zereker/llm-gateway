// Package adapter 定义协议转换层的核心抽象：完整 Adapter 接口 + EndpointConfig。
//
// pkg/domain 定义最小 Adapter 接口（用于 RequestContext 字段）；
// 本包扩展为完整接口，实现该完整接口即自动满足 domain.Adapter。
//
// 详见 docs/architecture/02-protocol-translation.md。
package adapter

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// Adapter 单个上游厂商的完整接入实现。
//
// 一个 Adapter 一个 Vendor；同一个 Adapter 可声明多个 Modality。
// 每次请求由 Factory 构造一个新实例（不复用、无状态污染）。
type Adapter interface {
	domain.Adapter // Vendor() + NewResponseSession()

	NativeProtocol() domain.Protocol
	SupportedModalities() []domain.Modality

	Init(c context.Context, ep EndpointConfig, env *domain.RequestEnvelope) error
	BuildURL() (string, error)
	BuildHeaders(req *http.Request) error
	TransformRequest() ([]byte, error)
}

// EndpointConfig Adapter.Init 的输入。
//
// 不直接传 *domain.Endpoint，是因为我们想保持 Adapter 与 schedule 解耦
// （Adapter 只关心 URL / 凭证 / 厂商专有字段，不关心调度元信息）。
// dispatch 层（pkg/retry.Executor 实现）负责字段拷贝。
type EndpointConfig struct {
	ID     string
	Vendor string
	URL    string
	APIKey domain.Secret // 自带 dump / log 屏蔽；调上游前用 APIKey.Reveal()
	Extra  json.RawMessage
}
