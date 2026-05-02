// Package scheduling 定义端点选择层的核心类型与接口。
//
// 详见 docs/architecture/03-endpoint-scheduling.md。
package scheduling

import "encoding/json"

// Endpoint ConfigStore 下发的单个上游接入点。
type Endpoint struct {
	ID     string
	Vendor string // 与 adapter.Vendor 对应
	URL    string // 上游 base URL
	APIKey string // 凭证（运行时按需脱敏 / 存到 secret store）
	Group  string // 与 identity.User.Group 匹配；默认 "default"
	Model  string // 该 endpoint 服务的模型名（与 ModelService.Model 对齐）
	Weight int    // 加权随机的基础权重；> 0
	RPM    int    // endpoint 层每分钟请求数硬上限
	TPM    int    // endpoint 层每分钟 token 硬上限
	RPS    int    // endpoint 层每秒请求数硬上限

	// 能力声明：决定 form 与可用调度器
	Capabilities EndpointCapabilities

	// 厂商专有配置，Adapter 自行解析
	Extra json.RawMessage
}

// EndpointCapabilities 能力标记。
type EndpointCapabilities struct {
	SelfHosted          bool   // true → FormSelfHosted；false → FormVendor
	KVMetricEndpoint    string // 自部署 KV / 队列深度 metric 抓取地址（空表示无）
	HealthProbeEndpoint string // 自部署主动 probe 地址（空表示无）
	PrefixCacheEnabled  bool   // 该 endpoint 是否参与 prefix cache 一致性哈希
}

// EndpointForm 由 Capabilities 派生。
type EndpointForm int

const (
	FormVendor     EndpointForm = iota // 第三方厂商（OpenAI、Anthropic、AWS Bedrock 等）
	FormSelfHosted                     // 自部署（vLLM、Ollama、SGLang 等内部可观测的部署）
)

func (f EndpointForm) String() string {
	if f == FormSelfHosted {
		return "self_hosted"
	}
	return "vendor"
}

// Form 派生方法。
func (e *Endpoint) Form() EndpointForm {
	if e.Capabilities.SelfHosted {
		return FormSelfHosted
	}
	return FormVendor
}
