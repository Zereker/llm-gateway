package ctx

import "encoding/json"

// Endpoint ConfigStore 下发的单个上游接入点。
type Endpoint struct {
	ID     string
	Vendor string // 与 adapter.Vendor 对应
	URL    string // 上游 base URL
	APIKey string // 凭证
	Group  string // 与 UserIdentity.Group 匹配；默认 "default"
	Model  string // 该 endpoint 服务的模型名
	Weight int    // 加权随机的基础权重；> 0
	RPM    int    // endpoint 层每分钟请求数硬上限
	TPM    int    // endpoint 层每分钟 token 硬上限
	RPS    int    // endpoint 层每秒请求数硬上限

	Capabilities EndpointCapabilities
	Extra        json.RawMessage // 厂商专有配置，Adapter 自行解析
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
