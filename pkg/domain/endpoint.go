package domain

import (
	"encoding/json"
	"time"
)

// Endpoint 上游接入点的业务模型（docs/06 §3）。
//
// 跟 repo.Endpoint 平行存在：repo.Endpoint 带 db: tag + Scanner/Valuer 处理 DB；
// domain.Endpoint 是 middleware/schedule/adapter 用的纯结构（无 SQL 依赖）。
// repo 通过 ToDomainEndpoint() 在 SQL 边界转换。
type Endpoint struct {
	ID      int64
	Name    string
	Vendor  string
	Model   string
	Group   string
	Weight  uint32
	Enabled bool

	// Protocol 这条 endpoint 上游说的协议（OpenAI / Anthropic / Gemini / Responses）。
	//
	// **必填**——零值 ProtoUnknown 会让 DefaultLookup.Get 直接返 nil，eligibility
	// filter 剔除该 endpoint。deployer 写 endpoint 时必须显式配。
	//
	// **设计动机**：协议是 endpoint 级属性，不是 vendor 级——同一 vendor 完全可能
	// 挂多条 endpoint 走不同协议（例如 Anthropic 同时提供原生 Messages API +
	// OpenAI-compatible API 时，两条 endpoint 分别 Protocol=Anthropic / Protocol=OpenAI）。
	//
	// **协议转换判定**：dispatcher 比较 (env.SourceProtocol, ep.Protocol)：
	//   - 相等 → identity translator 透传（无实际转换）
	//   - 不等 → translator.Find(src, ep.Protocol) 找跨协议翻译器；缺则 eligibility 剔除
	Protocol Protocol

	Auth         AuthConfig
	Routing      RoutingConfig
	Quota        QuotaConfig
	Capabilities EndpointCapabilities

	// Quirks endpoint 级 body / header 微调 DSL（pkg/protocol/quirks）。
	// 空 = no-op。combine.go 在 translator 之后、adapter BuildRequest 之前把 body
	// 和 header 一起跑完，再把 final body + headers 一次交给 adapter 组装。
	Quirks json.RawMessage

	Extra json.RawMessage

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// EndpointCapabilities endpoint 能力标记（modality + self-hosted 子领域）。
//
// **JSON 形态**（落 endpoints.capabilities 列；deployer SQL 写）：
//
//	{
//	  "modalities":         ["chat", "embedding"],
//	  "self_hosted":        false,
//	  "prefix_cache_enabled": true
//	}
type EndpointCapabilities struct {
	// Modalities 这条 endpoint 能承接的模态白名单（subset of vendor 能力）。
	//
	// **优先级**：非空时 eligibility 用本字段判断；为空时 fall back 到 vendor
	// Factory.Metadata().SupportedModalities（vendor 级"上限"）。
	//
	// **典型用法**：OpenAI vendor 同时声明 chat / embedding / image / audio，
	// 但 deployer 给一条 endpoint 只买了 gpt-4o chat quota，就 `["chat"]` 锁死，
	// 不让该 endpoint 误接 /v1/embeddings 之类请求。
	Modalities []Modality `json:"modalities,omitempty"`

	SelfHosted          bool   `json:"self_hosted,omitempty"`
	KVMetricEndpoint    string `json:"kv_metric_endpoint,omitempty"`
	HealthProbeEndpoint string `json:"health_probe_endpoint,omitempty"`
	PrefixCacheEnabled  bool   `json:"prefix_cache_enabled,omitempty"`
}

// EndpointForm 由 Capabilities 派生。
type EndpointForm int

const (
	FormVendor EndpointForm = iota
	FormSelfHosted
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

// =============================================================================
// AuthConfig / Auth payload types（vendor 鉴权 — docs/06 §3）
// =============================================================================

// AuthConfig vendor-tagged 鉴权配置。
//
// 注意：domain.AuthConfig **不**自带 Scanner/Valuer（生产侧 DB 加密在 repo 层做）。
// 业务代码统一通过 DecodePayload[T] 取 typed payload。
type AuthConfig struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// 已知 auth type 常量（payload struct 见下方）。
const (
	AuthTypeBearer    = "bearer"
	AuthTypeXAPIKey   = "x-api-key"
	AuthTypeGeminiKey = "gemini-key"
	AuthTypeAWSSigV4  = "aws-sigv4"
	AuthTypeOAuth2SA  = "oauth2-sa"
	AuthTypeVertexADC = "vertex-adc"
)

// BearerAuth: Authorization: Bearer <api_key>
type BearerAuth struct {
	APIKey string `json:"api_key"`
}

// XAPIKeyAuth: x-api-key: <api_key> 头（Anthropic）
type XAPIKeyAuth struct {
	APIKey string `json:"api_key"`
}

// GeminiAuth: ?key=<api_key> 或 x-goog-api-key 头（Gemini AI Studio）
type GeminiAuth struct {
	APIKey string `json:"api_key"`
}

// AWSSigV4Auth: AWS Signature Version 4（Bedrock）
type AWSSigV4Auth struct {
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	Region    string `json:"region"`
}

// OAuth2SAAuth: Service Account JSON 走 OAuth2 access token（Vertex AI）
type OAuth2SAAuth struct {
	ServiceAccountJSON string `json:"service_account_json"`
}

// VertexADCAuth: 走 Application Default Credentials
type VertexADCAuth struct {
	Scopes []string `json:"scopes,omitempty"`
}

// DecodePayload 把 AuthConfig.Payload 反序列化到 T。
//
//	bearer, err := domain.DecodePayload[domain.BearerAuth](ep.Auth)
//	if err != nil { ... }
//	req.Header.Set("Authorization", "Bearer "+bearer.APIKey)
func DecodePayload[T any](a AuthConfig) (T, error) {
	var t T
	if len(a.Payload) == 0 {
		return t, errEmptyAuthPayload
	}
	if err := json.Unmarshal(a.Payload, &t); err != nil {
		return t, err
	}
	return t, nil
}

// EncodePayload helper：构造 AuthConfig 时把 typed payload 序列化成 RawMessage。
func EncodePayload(authType string, payload any) (AuthConfig, error) {
	if authType == "" {
		return AuthConfig{}, errEmptyAuthType
	}
	if payload == nil {
		return AuthConfig{Type: authType}, nil
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return AuthConfig{}, err
	}
	return AuthConfig{Type: authType, Payload: b}, nil
}

// =============================================================================
// RoutingConfig / QuotaConfig（业务侧的纯结构）
// =============================================================================

// RoutingConfig vendor-specific URL / 端点定位字段（docs/02 §1 vendor 路由）。
type RoutingConfig struct {
	URL        string `json:"url,omitempty"`
	Region     string `json:"region,omitempty"`
	Project    string `json:"project,omitempty"`
	Location   string `json:"location,omitempty"`
	Publisher  string `json:"publisher,omitempty"`
	Deployment string `json:"deployment,omitempty"`
	APIVersion string `json:"api_version,omitempty"`
}

// QuotaConfig endpoint 上游限流硬约束（稀疏字段：nil = 不限）。
type QuotaConfig struct {
	RPM                *uint32 `json:"rpm,omitempty"`
	TPM                *uint32 `json:"tpm,omitempty"`
	RPS                *uint32 `json:"rps,omitempty"`
	ConcurrentRequests *uint32 `json:"concurrent_requests,omitempty"`
}

// IsEmpty 判断是否一个 quota 字段都没填。
func (q QuotaConfig) IsEmpty() bool {
	return q.RPM == nil && q.TPM == nil && q.RPS == nil && q.ConcurrentRequests == nil
}

// sentinel errors（avoid import "errors" in domain core types file）
type authErr string

func (e authErr) Error() string { return string(e) }

const (
	errEmptyAuthPayload authErr = "AuthConfig: empty payload"
	errEmptyAuthType    authErr = "AuthConfig: empty type"
)
