# 02 — Protocol Translation

本文定义协议转换层：`Adapter` 接口、`domain.RequestEnvelope` 数据结构、`Translator` 跨协议翻译、`ResponseSession` 流式响应聚合，以及 `ParamSpec` 同协议族内参数适配。

> **阅读前**：[01-request-pipeline](01-request-pipeline.md) 的 `domain.RequestContext` 和 M3 Envelope / M7 Schedule 契约。

## 1. 范围与目标

**范围**：从 HTTP body 进入网关那一刻，到将上游响应写回客户端为止，所有"协议层面"的解析、翻译、写出。

**目标**：

| # | 目标 | 成功判据 |
|---|------|---------|
| G1 | 一个厂商一个 `Adapter` | 无 `ChatAdapter` / `MessageAdapter` / `ImageAdapter` 平行接口 |
| G2 | 接入 OpenAI 兼容厂商零业务代码 | 新厂商一个文件 + 配置 |
| G3 | 客户端协议透传未知字段 | 任意未在 `CanonicalRequest` 中的字段都能原样传到上游 |
| G4 | 流式 / 非流式同一接口 | `ResponseSession.Feed` + `Finalize` 二选一无歧义 |
| G5 | 跨协议翻译独立成包 | `pkg/translator/` 可被多个 Adapter 引用 |
| G6 | 错误分类对齐限流 / 调度 | 上游 4xx/5xx 统一映射到 `domain.ErrorClass` |
| G7 | 注册表零 switch | `init()` + blank import 注册 |
| G8 | 多模态共存 | 一个 Adapter 可同时声明 `Chat` + `Image` + `Task` 多个 Modality |

## 2. 设计原则

| # | 原则 | 含义 |
|---|------|------|
| Q1 | **一个厂商一个 Adapter** | Adapter 与厂商一一对应；多模态由 `SupportedModalities()` 声明，不拆多个接口 |
| Q2 | **请求体双解析** | `Envelope.RawBytes` 用于上游透传，`Envelope.Parsed` 用于业务读取；杜绝结构化解析丢字段 |
| Q3 | **流式聚合统一到 Session** | 所有流式 / 非流式响应都走 `ResponseSession.Feed` + `Finalize`；无平行的 `StreamHandler` 接口 |
| Q4 | **跨协议翻译独立模块** | `pkg/translator/` 包统一处理 (src → dst) 双向翻译；Adapter 不内嵌翻译逻辑 |
| Q5 | **同族字段差异由 ParamSpec 处理** | 跨协议族走 Translator；同族（如多个 OpenAI 兼容厂商）的字段名 / 取值 / 必填扩展由 `ParamSpec` 声明式处理 |
| Q6 | **注册表 + init() 注册** | `adapter.Register("vendor", factory)` 通过 `init()` 调用；`main.go` 用 blank import 决定哪些厂商进二进制 |
| Q7 | **错误集中分类** | Adapter 自带 `Classify`；所有上游错误统一为 `domain.AdapterError{Class: ...}` |
| Q8 | **能力声明可选** | `CapabilityProvider` 接口预留；用于未来端点选择按"是否支持 thinking / tools / vision"过滤，本期不强制实现 |

## 3. 接口与数据结构

### 3.1 domain.RequestEnvelope

```go
// pkg/domain/envelope.go
package envelope

import "time"

// Envelope 是 M3 Envelope middleware 的产物，承载完整请求信息。
//
// 业务逻辑读 Parsed（结构化），透传到上游用 RawBytes（原始字节）。
// 这一双通道设计让"网关本身关心的字段（model / stream 等）"与
// "网关不关心但要保留的字段（reasoning_details / metadata 等）"完全解耦。
type Envelope struct {
    RawBytes       []byte           // 原始请求 body，未经任何修改
    Parsed         CanonicalRequest // 结构化解析（仅含网关关心的字段）
    SourceProtocol SourceProtocol   // 客户端使用的协议族
    Modality       Modality         // 请求模态
    RequestTime    time.Time        // M3 完成解析的时刻
}
```

### 3.2 CanonicalRequest

`CanonicalRequest` 是网关内部的"通用请求形态"——所有 Adapter 与 Translator 都以它为输入。它**只覆盖跨厂商共性的字段**，专有字段不进入 Canonical（在 `RawBytes` 中保留）。

```go
// pkg/domain/canonical.go
package envelope

import "encoding/json"

type CanonicalRequest struct {
    // 路由必需字段
    Model  string `json:"model"`
    Stream bool   `json:"stream"`

    // 通用聊天字段
    Messages    []Message `json:"messages,omitempty"`
    System      string    `json:"system,omitempty"` // Anthropic 风格的 system；OpenAI 风格走 Messages[0]
    MaxTokens   *int64    `json:"max_tokens,omitempty"`
    Temperature *float64  `json:"temperature,omitempty"`
    TopP        *float64  `json:"top_p,omitempty"`
    TopK        *int64    `json:"top_k,omitempty"`
    Stop        []string  `json:"stop,omitempty"`

    // 工具与结构化输出
    Tools          []Tool          `json:"tools,omitempty"`
    ToolChoice     *ToolChoice     `json:"tool_choice,omitempty"`
    ResponseFormat *ResponseFormat `json:"response_format,omitempty"`

    // 元信息
    User     string `json:"user,omitempty"`     // 客户端可选的终端用户标识
    Metadata map[string]string `json:"metadata,omitempty"`

    // 透传扩展（非 OpenAI 标准但常见）
    Reasoning *Reasoning `json:"reasoning,omitempty"` // OpenAI o-系 / DeepSeek 等
    Thinking  *Thinking  `json:"thinking,omitempty"` // Anthropic

    // 多模态
    Modalities []string `json:"modalities,omitempty"` // ["text", "audio"] 等
    Audio      *AudioOptions `json:"audio,omitempty"`

    // 兜底：未识别字段（仅元信息使用，业务不应依赖此字段；透传请用 RawBytes）
    Unknown json.RawMessage `json:"-"`
}

type Message struct {
    Role    string          `json:"role"` // system / user / assistant / tool
    Content json.RawMessage `json:"content"` // string 或 []ContentPart 都接受
    Name    string          `json:"name,omitempty"`

    // 工具调用
    ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
    ToolCallID string     `json:"tool_call_id,omitempty"`
}

type Tool struct {
    Type     string          `json:"type"` // 通常 "function"
    Function *ToolFunction   `json:"function,omitempty"`
}

// ToolChoice / ToolFunction / ResponseFormat / Reasoning / Thinking / AudioOptions
// 等结构定义略；遵循 OpenAI / Anthropic 公开规范。
```

**起步字段集**：26 个最常用字段；新增字段需在 PR 中讨论是否值得进 Canonical（vs 只在 RawBytes 里透传）。

### 3.3 SourceProtocol 与 Modality

```go
// pkg/domain/protocol.go
package envelope

type SourceProtocol int

const (
    ProtoUnknown   SourceProtocol = iota
    ProtoOpenAI                   // /v1/chat/completions, /v1/embeddings, /v1/images, ...
    ProtoAnthropic                // /v1/messages
    ProtoGemini                   // /v1beta/models/.../generateContent
    ProtoBedrock                  // AWS Bedrock 格式
    ProtoCustom                   // 厂商自定义；Adapter 自行解释
)

func (p SourceProtocol) String() string {
    switch p {
    case ProtoOpenAI:    return "openai"
    case ProtoAnthropic: return "anthropic"
    case ProtoGemini:    return "gemini"
    case ProtoBedrock:   return "bedrock"
    case ProtoCustom:    return "custom"
    default:             return "unknown"
    }
}
```

```go
// pkg/domain/modality.go
package envelope

type Modality int

const (
    ModalityChat      Modality = iota // 含 message 类（Anthropic Messages API）
    ModalityEmbedding
    ModalityImage                     // 含文生图、图生图、Inpaint，Adapter 内部按 Parsed 分发
    ModalityRerank
    ModalityTTS
    ModalityASR
    ModalityTask                      // 异步任务（视频生成、长音频合成等），轮询模型
)

func (m Modality) String() string {
    switch m {
    case ModalityChat:      return "chat"
    case ModalityEmbedding: return "embedding"
    case ModalityImage:     return "image"
    case ModalityRerank:    return "rerank"
    case ModalityTTS:       return "tts"
    case ModalityASR:       return "asr"
    case ModalityTask:      return "task"
    }
    return "unknown"
}
```

### 3.4 middleware.Detector / middleware.Parser

M3 middleware 注入这两个接口的实现（默认实现见 `pkg/domain/default/`）。

```go
// pkg/domain/detector.go
package envelope

// Detector 识别请求的协议族与模态。
// 默认实现按 URL 路径优先匹配（如 /v1/messages → Anthropic + Chat），body 特征兜底。
type Detector interface {
    Detect(path string, body []byte) (SourceProtocol, Modality)
}

// Parser 把 RawBytes 解析为 CanonicalRequest。
// 不同 SourceProtocol 用不同实现；Parser 内部按 SourceProtocol 分发。
type Parser interface {
    Parse(raw []byte, proto SourceProtocol, mod Modality) (CanonicalRequest, error)
}
```

**默认 Detector 优先级**：

```
1. URL 路径精确匹配（/v1/chat/completions, /v1/messages, /v1/embeddings, ...）
2. URL 路径前缀模糊匹配（/v1beta/models/* → Gemini）
3. body 特征（messages 字段 + role:"user" → OpenAI/Anthropic 二选一，按 max_tokens 字段名等区分）
```

> 路径优先因为客户端 SDK 几乎都按规范路径调用；body 特征仅兜底，且需打 metric 追踪命中率。

## 4. Adapter 接口

```go
// pkg/adapter/adapter.go
package adapter

import (
    "context"
    "net/http"

    "github.com/zereker/llm-gateway/pkg/domain"
    "github.com/zereker/llm-gateway/pkg/schedule"
)

// Adapter 是单个上游厂商的接入实现。
//
// 一个 Adapter 一个 Vendor；同一个 Adapter 可声明多个 Modality（Chat + Image + Task 等）。
// 每次请求由 Factory 构造一个新实例（不复用、无状态污染）。
type Adapter interface {
    // 元信息
    Vendor() string                              // "openai" / "anthropic" / "vllm" / ...
    NativeProtocol() domain.Protocol     // 该厂商上游使用的协议族
    SupportedModalities() []domain.Modality

    // 请求侧
    Init(ctx context.Context, ep *domain.Endpoint, env *domain.RequestEnvelope) error
    BuildURL() (string, error)
    BuildHeaders(req *http.Request) error
    TransformRequest() ([]byte, error)

    // 响应侧
    NewResponseSession() ResponseSession
}
```

> Adapter 实例由 Factory 构造一次（M7 内）；后续方法调用都在同一实例上，可缓存中间结果（如 `Init` 解析 Endpoint 凭证一次）。

### 4.1 Factory 与 Registry

```go
// pkg/adapter/registry.go
package adapter

import "fmt"

type Factory func() Adapter

var registry = map[string]Factory{}

// Register 注册一个 Adapter 工厂。各 Adapter 包通过 init() 调用。
func Register(vendor string, f Factory) {
    if _, ok := registry[vendor]; ok {
        panic(fmt.Sprintf("adapter: vendor %q already registered", vendor))
    }
    registry[vendor] = f
}

// Get 根据 vendor 名取出工厂；未注册则返回 nil。
func Get(vendor string) Factory {
    return registry[vendor]
}

// Vendors 返回当前已注册的厂商列表（启动时与配置中心比对，发现漏注册可告警）。
func Vendors() []string {
    out := make([]string, 0, len(registry))
    for v := range registry {
        out = append(out, v)
    }
    return out
}
```

```go
// pkg/adapter/openai/adapter.go (示例厂商包)
package openai

import (
    "github.com/zereker/llm-gateway/pkg/adapter"
)

func init() {
    adapter.Register("openai", func() adapter.Adapter {
        return &Adapter{}
    })
}

type Adapter struct {
    // ... 实现 adapter.Adapter 接口
}
```

```go
// cmd/gateway/main.go
import (
    _ "github.com/zereker/llm-gateway/pkg/adapter/openai"
    _ "github.com/zereker/llm-gateway/pkg/adapter/anthropic"
    _ "github.com/zereker/llm-gateway/pkg/adapter/google"
    _ "github.com/zereker/llm-gateway/pkg/adapter/aws_bedrock"
    _ "github.com/zereker/llm-gateway/pkg/adapter/azure_openai"
    _ "github.com/zereker/llm-gateway/pkg/adapter/vllm"
    _ "github.com/zereker/llm-gateway/pkg/adapter/ollama"
    // 部署方按需选择 import 哪些厂商
)
```

#### 包粒度约定：一家 vendor 一个 package

| 反例 | 正例 |
|------|------|
| 把 OpenAI / Azure OpenAI / DeepSeek / Mistral / Ollama-OpenAI / vLLM-OpenAI 全塞进 `pkg/adapter/openai/`，差异由内部分支处理 | 每家独立成包：`pkg/adapter/openai`、`pkg/adapter/azure_openai`、`pkg/adapter/deepseek`、`pkg/adapter/mistral`、`pkg/adapter/ollama`、`pkg/adapter/vllm` |

**理由**：

- **差异天然增长**：今天看起来 OpenAI 兼容的厂商，明天就会出现 reasoning 字段名差异、tool-call 行为差异、自家专有 header 等。共用包会让 `openai/adapter.go` 长成"if vendor=='deepseek' else if vendor=='mistral'"链。
- **可独立裁剪**：`cmd/gateway/main.go` 用 blank import 选择构建哪些 vendor。一家一包，可以精确决定 binary 体积与依赖面。
- **配置与 Registry 对齐**：`adapter.Register("<vendor>", ...)` 的 key 就是包名，配置中的 `vendor: deepseek` 直接对应 `_ "pkg/adapter/deepseek"`，无歧义。
- **测试边界清晰**：每个 vendor 包独立 `adapter_test.go`，不必通过 `if vendor==X` 分支选择 fixture。

**复用机制**：差异化不靠包合并，而是靠**接口默认实现 + 内嵌**：

```go
// pkg/adapter/openai_session/default.go  —— 默认 ResponseSession 实现，OpenAI 兼容厂商内嵌使用
package openai_session
func NewDefault(...) adapter.ResponseSession { ... }

// pkg/adapter/deepseek/adapter.go
func (a *Adapter) NewResponseSession(...) adapter.ResponseSession {
    return openai_session.NewDefault(...)   // 复用，不继承
}
```

> 例外：纯命名别名（如 `azure_openai` 仅 URL 模板和 header 略不同）可考虑由 `openai` 包工厂带参生成；但**只要存在协议字段层面差异（哪怕只有一个），一律独立包**。

### 4.2 ResponseSession 接口

```go
// pkg/adapter/session.go
package adapter

import (
    "github.com/zereker/llm-gateway/pkg/domain"
    "github.com/zereker/llm-gateway/pkg/usage"
)

// ResponseSession 处理上游响应（流式 / 非流式统一）。
//
// 非流式调用：
//   sess.Feed(fullBody)            // 一次喂完
//   u, resp, err := sess.Finalize()
//
// 流式调用：
//   for chunk := range upstream {
//       out, err := sess.Feed(chunk)
//       writer.Write(out)          // 实时写给客户端
//   }
//   u, resp, err := sess.Finalize()
//
// out 是"翻译 / 加工后写给客户端的字节"；非流式时通常返回空（客户端在 Finalize 后拿完整 resp）。
type ResponseSession interface {
    Feed(chunk []byte) ([]byte, error)
    Finalize() (*domain.Usage, *domain.CanonicalResponse, *domain.AdapterError)
}
```

**Session 内部职责**：
- 累积 chunk 形成完整响应
- 提取 `Usage`（委托给 `usage.TokenExtractor`，详见 [05]）
- 跨协议时反向翻译（如上游是 OpenAI、客户端用的 Anthropic 协议，Session 要把 OpenAI chunk 翻译成 Anthropic 事件流）
- 错误分类（包装 `domain.AdapterError`）

### 4.3 CanonicalResponse

```go
// pkg/domain/canonical_response.go
package envelope

import "encoding/json"

// CanonicalResponse 是 ResponseSession.Finalize 的"中间表示"，仅在跨协议时使用。
// 同协议时 Session 通常直接把上游 chunk 原样写出，不构造 CanonicalResponse。
type CanonicalResponse struct {
    ID      string    `json:"id"`
    Model   string    `json:"model"`
    Created int64     `json:"created"`
    Choices []Choice  `json:"choices"`
    Usage   json.RawMessage `json:"usage,omitempty"`
    // 透传上游原始 body 用于 debug
    Raw json.RawMessage `json:"-"`
}

type Choice struct {
    Index        int             `json:"index"`
    Message      *Message        `json:"message,omitempty"`        // 非流式
    Delta        *Message        `json:"delta,omitempty"`          // 流式
    FinishReason string          `json:"finish_reason,omitempty"`
    Logprobs     json.RawMessage `json:"logprobs,omitempty"`
}
```

## 5. Translator 跨协议翻译

```go
// pkg/translator/translator.go
package translator

import "github.com/zereker/llm-gateway/pkg/domain"

// Translator 把请求 / 响应在两个协议族之间双向翻译。
//
// 注意：每个 Translator 是单向的（src → dst）。需要双向翻译时实例化两个。
type Translator interface {
    // 请求翻译：返回发给 dst 上游的字节
    TranslateRequest(env *domain.RequestEnvelope) ([]byte, error)

    // 响应翻译（非流式）：把 dst 协议的响应翻译回 src 协议
    TranslateResponse(resp *domain.CanonicalResponse) (*domain.CanonicalResponse, error)

    // 响应翻译（流式 chunk）：把 dst 协议的 chunk 翻译为 src 协议的 chunk
    // 如 OpenAI SSE chunk → Anthropic event chunk
    TranslateStreamChunk(chunk []byte) ([]byte, error)
}
```

```go
// pkg/translator/registry.go
package translator

import "github.com/zereker/llm-gateway/pkg/domain"

type key struct {
    Src domain.Protocol
    Dst domain.Protocol
}

var registry = map[key]Translator{}

func Register(src, dst domain.Protocol, t Translator) {
    registry[key{src, dst}] = t
}

// Get 返回 (src → dst) 的 Translator。src == dst 时返回 identityTranslator（透传）。
func Get(src, dst domain.Protocol) Translator {
    if src == dst {
        return identityTranslator{}
    }
    return registry[key{src, dst}]
}

type identityTranslator struct{}

func (identityTranslator) TranslateRequest(env *domain.RequestEnvelope) ([]byte, error) {
    return env.RawBytes, nil
}
func (identityTranslator) TranslateResponse(r *domain.CanonicalResponse) (*domain.CanonicalResponse, error) {
    return r, nil
}
func (identityTranslator) TranslateStreamChunk(c []byte) ([]byte, error) { return c, nil }
```

**实现示例**：

```go
// pkg/translator/anthropic_to_openai.go
package translator

import "github.com/zereker/llm-gateway/pkg/domain"

func init() {
    Register(domain.ProtoAnthropic, domain.ProtoOpenAI, &anthropicToOpenAI{})
    Register(domain.ProtoOpenAI, domain.ProtoAnthropic, &openAIToAnthropic{})
}

type anthropicToOpenAI struct{}

func (anthropicToOpenAI) TranslateRequest(env *domain.RequestEnvelope) ([]byte, error) {
    // 1. 把 Anthropic 的 system 字段转入 OpenAI messages[0].role=system
    // 2. 字段重命名 / 类型转换（如 max_tokens 必填）
    // 3. 未识别字段从 RawBytes 中 sjson 取出后透传
    // 4. 输出 OpenAI 兼容 body
    // ...
}
// ... 同理 TranslateResponse / TranslateStreamChunk
```

> 每个 Translator 必须配套字段覆盖率测试（见第 10 章）。

## 6. Adapter.TransformRequest 完整流程

把 Translator + ParamSpec + 公共补洞串起来：

```
输入：env (含 RawBytes, Parsed, SourceProtocol, Modality)
      adapter.NativeProtocol

Step 1: 跨协议翻译（仅当 SourceProtocol != NativeProtocol）
  body = translator.Get(SourceProtocol, NativeProtocol).TranslateRequest(env)
  否则: body = env.RawBytes

Step 2: 同族字段适配（adapter.ParamSpec() != nil 时）
  spec = adapter.ParamSpec()
  body = applyParamMapping(body, spec.ParamMapping)             // 字段重命名
  body = filterUnsupported(body, spec.SupportedParams, mode)    // 过滤未知（按 mode）
  body = injectExtensions(body, spec.ProviderExtensions)         // 补齐专有字段
  body = validate(body, spec.Validators)                          // 范围校验 / 裁剪

Step 3: 公共补洞
  if adapter.NativeProtocol == ProtoOpenAI && env.Parsed.Stream:
      body = sjson.SetBytes(body, "stream_options.include_usage", true)

输出：发给上游的 bytes
```

## 7. ParamSpec 同协议族参数适配

```go
// pkg/adapter/paramspec.go
package adapter

// ParamSpec 描述一个 Adapter 在"协议族内部"的字段差异。
// 跨协议族的差异由 translator 处理；同族（如多个 OpenAI 兼容厂商）的字段名 /
// 取值范围 / 必填扩展由 ParamSpec 声明。
type ParamSpec struct {
    SupportedParams    map[string]bool          // 白名单：该上游支持的参数
    ParamMapping       map[string]string        // canonical 字段 → 上游字段名
    ProviderExtensions map[string]any           // 自动注入的上游专有字段
    Validators         map[string]ParamValidator // 取值范围校验 / 裁剪
}

type ParamValidator interface {
    Validate(value any) (newValue any, err error)
}

// 内置 Validator
type RangeValidator struct {
    Min, Max float64
    OnOver   OverflowMode // Reject / Clamp
}

type OverflowMode int

const (
    Reject OverflowMode = iota // 返回错误
    Clamp                      // 截断到范围内
)
```

**Adapter 可选实现**：

```go
type ParamSpecProvider interface {
    ParamSpec() *ParamSpec
}
```

未实现时 = 全透传（即 passthrough 行为）。

### 7.1 未知参数行为模式

| 模式 | 行为 | 默认 | 客户端覆盖 |
|------|------|-----|-----------|
| `drop` | 静默丢弃 + warning 日志 | ✅ | — |
| `strict` | 不支持参数 → 4xx 返回 | | Header `X-Unsupported-Params: strict` |
| `passthrough` | 原样发上游 | | Header `X-Unsupported-Params: passthrough` |

### 7.2 ParamSpec 示例

```go
// pkg/adapter/anthropic/paramspec.go
package anthropic

import "github.com/zereker/llm-gateway/pkg/adapter"

func (*Adapter) ParamSpec() *adapter.ParamSpec {
    return &adapter.ParamSpec{
        SupportedParams: map[string]bool{
            "model": true, "messages": true, "max_tokens": true,
            "stream": true, "temperature": true, "top_p": true,
            "top_k": true, "tools": true, "tool_choice": true,
            "system": true, "thinking": true,
        },
        ParamMapping: map[string]string{}, // Anthropic 字段名与 Canonical 一致
        ProviderExtensions: map[string]any{
            // anthropic-version 通过 Header 注入，不在 body
        },
        Validators: map[string]adapter.ParamValidator{
            "temperature": adapter.RangeValidator{Min: 0, Max: 1, OnOver: adapter.OverflowClamp},
            "top_p":       adapter.RangeValidator{Min: 0, Max: 1, OnOver: adapter.OverflowClamp},
        },
    }
}
```

## 8. 错误分类（对接 domain.ErrorClass）

```go
// pkg/adapter/classifier.go
package adapter

import "github.com/zereker/llm-gateway/pkg/domain"

// Classifier 把上游 HTTP 状态 + body 映射到 domain.ErrorClass。
//
// Adapter 可实现 Classifier 以接管特定厂商的 error schema；未实现时走 DefaultClassifier。
type Classifier interface {
    Classify(httpStatus int, body []byte) *domain.AdapterError
}

// DefaultClassifier 仅按 HTTP 状态分类。
type DefaultClassifier struct{}

func (DefaultClassifier) Classify(httpStatus int, body []byte) *domain.AdapterError {
    e := &domain.AdapterError{
        HTTPStatus:      httpStatus,
        UpstreamMessage: string(body),
    }
    switch {
    case httpStatus == 429:
        e.Class = domain.ErrRateLimit
    case httpStatus == 401, httpStatus == 403:
        e.Class = domain.ErrPermanent
    case httpStatus >= 400 && httpStatus < 500:
        e.Class = domain.ErrInvalid
    case httpStatus >= 500:
        e.Class = domain.ErrTransient
    default:
        e.Class = domain.ErrUnknown
    }
    return e
}
```

每个 Adapter 注册时声明自己的 Classifier（可选）：

```go
// pkg/adapter/openai/classifier.go
package openai

import "github.com/zereker/llm-gateway/pkg/adapter"

func (*Adapter) Classifier() adapter.Classifier {
    return openaiClassifier{}
}
```

**`domain.ErrorClass` 与重试策略对接**：详见 [03-endpoint-scheduling](03-endpoint-scheduling.md)。

| `domain.ErrorClass` | RetryExecutor 行为 |
|---|---|
| `Transient` | L1 同 endpoint retry → L2 fallback |
| `RateLimit` | L1 短退避 → L2 触发 cooldown 后 fallback |
| `Permanent` | L2 直接 fallback + cooldown |
| `Invalid` | 不重试，原样返回客户端 |

## 9. 模态特定接口

### 9.1 Task 模态（异步任务，轮询型）

```go
// pkg/adapter/task.go
package adapter

import (
    "context"
    "time"

    "github.com/zereker/llm-gateway/pkg/domain"
    "github.com/zereker/llm-gateway/pkg/usage"
)

// TaskAdapter 是 Task 模态 Adapter 必须额外实现的差异 Hook。
// 通用的"提交 → 轮询 → 超时"流程由 BaseTaskFlow 实现。
type TaskAdapter interface {
    BuildSubmitRequest(env *domain.RequestEnvelope) ([]byte, error)
    ExtractTaskID(submitResp []byte) (string, error)
    BuildQueryURL(taskID string) string
    ParseTaskStatus(queryResp []byte) (TaskStatus, *domain.Usage, error)
}

type TaskStatus int

const (
    TaskRunning TaskStatus = iota
    TaskSucceeded
    TaskFailed
    TaskCanceled
)

// BaseTaskFlow 通用任务执行流程。
type BaseTaskFlow struct {
    Adapter         TaskAdapter
    SubmitTimeout   time.Duration
    PollInterval    time.Duration
    MaxPollDuration time.Duration
}

func (f *BaseTaskFlow) Run(
    ctx context.Context,
    env *domain.RequestEnvelope,
) (resp []byte, u *domain.Usage, err *domain.AdapterError) {
    // 1. 提交任务
    // 2. 解析 task ID
    // 3. 循环轮询直到 Succeeded / Failed / Timeout
    // 4. 期间每次 poll 都更新 Usage（如有）
    // 5. 返回最终 response + Usage
}
```

### 9.2 Image 模态

无需独立接口；通过 Adapter 的 `Init` + `BuildURL` + `BuildHeaders` + `TransformRequest` 完成。
是文生图、图生图、Inpaint 由 `env.Parsed`（如 `prompt` / `image` / `mask` 字段）区分；Adapter 内部按字段分发到不同上游 endpoint。

### 9.3 Embedding / Rerank / TTS / ASR

同 Image：复用主 `Adapter` 接口；Modality 由 `env.Modality` 标识，Adapter 内部按 Modality 分发。

## 10. ModelCapabilities（可选，预留接口）

```go
// pkg/adapter/capabilities.go
package adapter

import "github.com/zereker/llm-gateway/pkg/domain"

type ModelCapabilities struct {
    MaxContextTokens      int
    SupportsThinking      bool
    SupportsTools         bool
    SupportsVision        bool
    SupportsStructuredOut bool
    SupportsMultiTurn     bool
    MaxToolCalls          int
    SupportedModalities   []domain.Modality
}

// CapabilityProvider 由 Adapter 可选实现。
// 端点选择层（[03]）在做"按能力过滤"时调用；未实现时不过滤。
type CapabilityProvider interface {
    ModelCapabilities(model string) *ModelCapabilities
}
```

> **本期不强制实现**。当端点选择需要"按能力过滤"成为运营诉求时，再批量实现。

## 11. 数据流时序

### 11.1 Chat 同协议（OpenAI 客户端 → OpenAI 兼容上游）

```
1. M3 Envelope 解析：env.SourceProtocol = ProtoOpenAI, env.Modality = ModalityChat
2. M5 ModelService 加载 → rc.ModelService
3. M6 Limit 预检通过
4. M7 Schedule:
   4.1 Scheduler 选出 endpoint（Vendor = "openai_compat_xxx"）
   4.2 factory := adapter.Get(endpoint.Vendor)
   4.3 a := factory()
   4.4 a.Init(ctx, endpoint, env)
   4.5 url, _ := a.BuildURL()
   4.6 a.BuildHeaders(req)
   4.7 body, _ := a.TransformRequest()
       // SourceProtocol == NativeProtocol == ProtoOpenAI
       // → identityTranslator + ParamSpec + 补洞
   4.8 http.Do(url, headers, body)
   4.9 sess := a.NewResponseSession()
   4.10 流式：for chunk { out, _ := sess.Feed(chunk); writer.Write(out) }
        非流式：sess.Feed(fullBody)
   4.11 u, resp, e := sess.Finalize()
   4.12 rc.Usage = u; rc.Error = e
5. M10 Tracing 异步发 Usage 事件
```

### 11.2 Chat 跨协议（Anthropic 客户端 → OpenAI 兼容上游）

```
1. M3: env.SourceProtocol = ProtoAnthropic, NativeProtocol = ProtoOpenAI
2-3 同上
4. M7 Schedule:
   4.7 a.TransformRequest():
       tr := translator.Get(ProtoAnthropic, ProtoOpenAI)
       body, _ := tr.TranslateRequest(env)  // 含未知字段透传
       // 然后 ParamSpec + 补洞
   4.9 sess := a.NewResponseSession()
       // Session 内部持有反向 translator (ProtoOpenAI → ProtoAnthropic)
       // Feed: 解析 OpenAI chunk → 翻译为 Anthropic event → 写出
   4.11 sess.Finalize():
       resp 翻译为 Anthropic 形态
       Usage 由 OpenAI Extractor 提取（详见 [05]）
```

### 11.3 Task 模态（异步任务）

```
1-3 同上，env.Modality = ModalityTask
4. M7 Schedule:
   4.5-4.8 跳过（Task 不走单次 HTTP）
   4.9 a 实现了 TaskAdapter 接口；BaseTaskFlow.Run 接管：
       - 提交任务（BuildSubmitRequest + http.Post）
       - 解析 task ID
       - 循环轮询（BuildQueryURL + http.Get + ParseTaskStatus）
       - 直到 Succeeded / Failed / Timeout
       - Usage 由 ParseTaskStatus 在每次 poll 时更新
5. M10 同上
```

### 11.4 错误路径

```
上游返回 429 / 5xx / 4xx
  ↓
sess.Finalize() 调 a.Classifier().Classify(httpStatus, body) → *domain.AdapterError
  ↓
domain.ErrorClass 传给 RetryExecutor（[03]）：
  Transient → L1 同 endpoint retry
  RateLimit → L1 短退避 + L2 fallback + endpoint cooldown
  Permanent → L2 fallback + endpoint cooldown
  Invalid   → 不重试，rc.Error 写入，M9 写出 4xx
```

## 12. 扩展场景

### 场景 A — 接入 OpenAI 兼容新厂商

1. 新建 `pkg/adapter/<vendor>/adapter.go`
2. `init()` 调 `adapter.Register("<vendor>", factory)`
3. 实现 `Vendor()` / `NativeProtocol()` / `SupportedModalities()` / `BuildURL()` / `BuildHeaders()`；`TransformRequest()` 通常用默认实现（identity translator + ParamSpec）；`NewResponseSession()` 通常用 `pkg/adapter/openai_session.NewDefault()`
4. `cmd/gateway/main.go` 加 blank import

**改动**：1 个文件，~50-80 行。

### 场景 B — 接入新协议厂商（如 Cohere）

1. 同 A，新建 `pkg/adapter/cohere/`
2. 加 `pkg/translator/openai_to_cohere.go` + `cohere_to_openai.go`，`init()` 调 `translator.Register`
3. blank import

**改动**：2 个文件，~150-200 行。

### 场景 C — 接入新模态（如音频分离）

1. 在 `domain.Modality` 加一项
2. Adapter 声明 `SupportedModalities()` 包含新项
3. 视模态需要，加专用接口（如 Task 那样的差异 Hook）

**改动**：1-3 个文件。

### 场景 D — 给某模型加 Anthropic 入口（已有 OpenAI 入口）

**零改动**。Adapter 不绑入口协议；M3 Envelope 识别 `SourceProtocol`，M7 在调用 Adapter 时自动走 Translator。

### 场景 E — 厂商专有字段（如 `reasoning_split`）

**零改动**。字段在 `env.RawBytes` 中原样保留，TransformRequest 透传到上游。
若网关内部需要读这个字段做业务决策，再考虑加入 `CanonicalRequest` 或通过 `env.Extras`。

## 13. 可观测性

```
adapter.request_total{vendor, modality}
adapter.request_duration_ms{vendor, modality, quantile}
adapter.error_total{vendor, class}                     # domain.ErrorClass
adapter.translate_total{src_proto, dst_proto}
adapter.translate_duration_ms{src_proto, dst_proto, quantile}

session.feed_chunks_total{vendor, modality}
session.finalize_success_rate{vendor, modality}
session.usage_extracted_total{vendor, modality}
```

trace 字段（写入 `rc.SchedulingDecision.Attempts[i]`）：

- `adapter.vendor`
- `adapter.native_protocol`
- `envelope.source_protocol`
- `envelope.modality`
- `translate_path`（如 `anthropic→openai→anthropic`）

## 14. 测试策略

| 测试层 | 内容 |
|--------|------|
| 单元 | 每个 Adapter 的 `BuildURL` / `BuildHeaders` / `TransformRequest` / `Classifier` |
| 单元 | 每个 Translator 的 `TranslateRequest` / `TranslateResponse` / `TranslateStreamChunk` |
| 字段覆盖率 | Translator 测试中覆盖已知字段 + 透传任意未知字段（断言透传保留） |
| 集成 | 一个 fake 上游 + Adapter 全链路（M7 调用 Adapter → Session → 响应） |
| 黄金 | 录制真实上游 chunk → 重放走 Session.Feed，断言输出 |

`字段覆盖率测试` 模板：

```go
// pkg/translator/anthropic_to_openai_test.go
func TestTranslateRequest_PreservesUnknownFields(t *testing.T) {
    raw := []byte(`{
      "model": "claude-3-5-sonnet",
      "messages": [...],
      "max_tokens": 100,
      "experimental_extra_field": "should be preserved"
    }`)
    env := &domain.RequestEnvelope{
        RawBytes:       raw,
        Parsed:         /* 解析后 */,
        SourceProtocol: domain.ProtoAnthropic,
    }
    out, err := (&anthropicToOpenAI{}).TranslateRequest(env)
    require.NoError(t, err)
    assert.Contains(t, string(out), "experimental_extra_field")
}
```

## 15. 演进规则

- **新增厂商**：新建 `pkg/adapter/<vendor>/` 包，`init()` 注册；不改本文档
- **新增协议族**：在 `domain.Protocol` 加常量；更新本文档第 3.3 节；为相关厂商加 Translator
- **新增 Modality**：在 `domain.Modality` 加常量；更新本文档第 3.3 节；视需要在第 9 章加专用接口
- **修改 Adapter 接口**：必须更新本文档第 4 章；评估对所有现有厂商实现的影响
- **修改 ParamSpec / Validator**：更新本文档第 7 章；新 Validator 内置实现放 `pkg/adapter/`
- **修改错误分类**：必须同时更新本文档第 8 章 与 [01-request-pipeline](01-request-pipeline.md) 第 7 章
