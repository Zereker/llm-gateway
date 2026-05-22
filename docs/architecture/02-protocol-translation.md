# 02 — Protocol Translation

本文记录协议转换的抽象与组合：客户端协议进、上游协议出（pre-call），上游
响应回、客户端协议出（post-call）。两阶段封装在 `pkg/protocol.Handler` facade
之下；内部仍由 `pkg/adapter`（vendor HTTP 层）+ `pkg/translator`（body shape 层）
两个独立子抽象组成，但消费侧只看 Handler。

核心原则：

- 协议归属是 **endpoint 级**属性（`Endpoint.Protocol`），不是 vendor 级。
- Handler 是 (endpoint, sourceProtocol) 二元组的端到端处理器；按请求**动态组合**，
  不在 init() 静态注册矩阵。
- 不追求补齐任意 `source × target` 协议矩阵——没有注册的组合直接视为不支持，
  eligibility 过滤剔除该 endpoint，请求要么走 fallback 要么 503。

## 1. 抽象关系

```text
┌──────────────────────────────────────────────────────────────────┐
│ pkg/protocol.Handler  (facade，消费侧只看它)                       │
│                                                                  │
│   ┌──────────────────────────┐  ┌────────────────────────────┐   │
│   │ pkg/adapter.Factory      │  │ pkg/translator.Translator  │   │
│   │ (vendor HTTP 层)         │  │ (body shape 转换 + usage)   │   │
│   │  - Metadata              │  │  - Source / Target          │   │
│   │  - NewSession            │  │  - TranslateRequest         │   │
│   │  - Session.BuildRequest  │  │  - NewResponseHandler       │   │
│   └──────────────────────────┘  └────────────────────────────┘   │
└──────────────────────────────────────────────────────────────────┘
                 ▲
                 │ Combine(ad, tr) → Handler
                 │
        DefaultLookup.Get(ep, srcProto) 在请求时动态组合
```

## 2. 端到端请求流水线

```text
Client request
  ↓
M3 Envelope: 写 rc.Envelope (RawBytes / SourceProtocol / Modality)
            + rc.Handlers = protocol.DefaultLookup{}
  ↓
M5 ModelService: 解析 model + fallback chain
  ↓
M7 Schedule → dispatch.Dispatcher.Dispatch(ctx, w, rc):
  loop {
    ep := Selector.Select(query)                                    // StageSelect
    handler := rc.Handlers.Get(ep, env.SourceProtocol)               // 动态组合 Handler
    if handler == nil { record StagePrepare; retry / fallback }
    
    invocation := InvokerFactory.For(ep, env, body, handler)
    res := invocation.Invoke(ctx)
      └─ reserve quota                                              // StageReserve
      └─ handler.PrepareCall(ep, srcBody) → Call{Request, UpstreamBody}  // StagePrepare
      └─ client.Do(req)                                             // StageInvoke
    
    if success: res.StreamTo(ctx, w)
      └─ handler.NewResponseStream().Feed/Flush — chunk-by-chunk 翻译回客户端协议
  }
```

## 3. `domain.Endpoint.Protocol`

**必填字段**。deployer 创建 endpoint 时（SQL INSERT）显式声明该 endpoint 上游说什么协议
（`openai` / `anthropic` / `gemini` / `responses` / ...）；缺失或 `ProtoUnknown`
时 `DefaultLookup.Get` 返回 nil，eligibility 剔除该 endpoint。

```go
type Endpoint struct {
    ...
    Vendor   string             // openai|anthropic|gemini|ark|... — vendor 适配器选择
    Protocol domain.Protocol    // openai|anthropic|gemini|responses|... — 协议归属
    ...
}
```

**为什么协议是 endpoint 级而不是 vendor 级**：同一 vendor 可以同时挂多条不同协议
的 endpoint。例：

| vendor | endpoint.Protocol | translator 需要 |
|---|---|---|
| anthropic | anthropic | (Anthropic → Anthropic) identity |
| anthropic | openai     | (OpenAI → Anthropic) cross |
| openai | openai | (OpenAI → OpenAI) identity |
| openai | responses | (OpenAI → Responses) — n/a 实际倒过来 |
| openai | anthropic | (Anthropic → Anthropic 仅在 vendor 跑 anthropic 兼容 API 时) |

vendor adapter 不再声明 `NativeProtocol`——它只知道 HTTP 层细节（auth header /
URL / TLS）；协议归属交给 endpoint。

## 4. `pkg/protocol.Handler` — facade

```go
type Handler interface {
    Capabilities() Capabilities

    // pre-call：translate srcBody + 套 vendor HTTP 信封
    PrepareCall(ctx, ep, srcBody) (*Call, error)

    // post-call：chunk-by-chunk 翻译响应给客户端
    NewResponseStream() ResponseStream
}

type Call struct {
    Request      *http.Request  // 已准备好发往上游的 HTTP 请求
    UpstreamBody []byte         // 翻译后的字节副本（给 audit/hook 看）
}

type Capabilities struct {
    SourceProtocol      domain.Protocol   // = translator.Source()
    UpstreamProtocol    domain.Protocol   // = translator.Target() == ep.Protocol
    SupportedModalities []domain.Modality // = adapter.Metadata().SupportedModalities
}
```

**Capabilities 不带 Vendor**——Vendor 是 endpoint 的属性，不是 Handler 的；
Handler 是 (adapter, translator) 动态组合，跟 specific endpoint 在 `PrepareCall`
入参时才挂钩。

## 4a. Quirks — endpoint 级 request 微调（`pkg/protocol/quirks`）

`translator` 只负责"客户端协议 → 上游协议"的形状转换；同一上游协议内不同 vendor /
模型仍有细微差异。**所有 quirks 都是 deployment 知识，存在 `endpoints.quirks` JSON 列，
deployer 直接 SQL 配置**——不在代码里 init() 注册任何 vendor 规则。

两类典型差异：

**body 字段**
- OpenAI o1/o3/o4 推理模型：`max_tokens` → `max_completion_tokens`；strip
  `temperature` / `top_p` / `presence_penalty` / `frequency_penalty`
- DeepSeek `deepseek-reasoner`：类似限制
- Anthropic Claude 3.7+ extended_thinking：插 `thinking` 块 + 强制 `temperature=1`
- vLLM / Ollama：strip 某些 OpenAI 特有字段

**header 字段**
- 不同 vendor 的 trace-id header 名不一样（`X-Request-Id` / `X-Ark-Request-Id` /
  `x-ds-request-id` 等）——gateway 统一用 `X-Request-Id`，deployer 配 rename
  让上游收到自己认的 header 名
- vendor 私有 header（如 `X-API-Version`）在 endpoint 上配死

插在 translator 与 adapter 之间（body）+ adapter 之后（header）：

```text
client body
  → translator.TranslateRequest   （客户端协议 → 上游协议 shape）
  → ep.Quirks.RewriteBody         （endpoint 配置的 body 微调）   ← 4a
  → adapter.BuildRequest          （HTTP 信封：URL / auth / Content-Type）
  → ep.Quirks.RewriteHeader       （endpoint 配置的 header 微调） ← 4a
  → upstream
```

**DSL**（存 `endpoints.quirks` JSON 列）：

```json
{
  "body": {
    "rename":      {"max_tokens": "max_completion_tokens"},
    "strip":       ["temperature", "top_p"],
    "set":         {"reasoning_effort": "high"},
    "set_default": {"max_completion_tokens": 4096}
  },
  "headers": {
    "rename":      {"X-Request-Id": "X-Ark-Trace-Id"},
    "strip":       ["X-Internal-Debug"],
    "set":         {"X-Custom-Tag": "prod"},
    "set_default": {"User-Agent": "llm-gateway/1.0"}
  }
}
```

应用顺序在 body / headers 子段内固定：`rename → strip → set → set_default`
（先腾位置、再清理、再覆写、最后兜底）。

接口（`pkg/protocol/quirks/quirks.go`）：

```go
type Rewriter interface {
    RewriteBody(body []byte) ([]byte, error)
    RewriteHeader(h http.Header)
}

// 编译 endpoint.Quirks JSON → Rewriter；strict mode（typo 字段直接报错）。
func CompileJSON(specJSON []byte) (Rewriter, error)
```

**combine.go 缓存**：同 spec 字面量（`string(ep.Quirks)`）只 compile 一次，
跨请求共享同一个 Rewriter；不同 endpoint 配置相同 quirks 时也共享。

**列 NULL / 空 JSON / `{}`** → no-op Rewriter，零开销。

**deployer 错配处理**：spec JSON 解析失败（或未知字段 typo）会让该 endpoint
的请求返 `PhaseQuirks` PrepareError（`dispatch.ClassInvalid`），dispatcher 直接
abort 不重试。配错的 endpoint 永远报错，pin 到 metric / log 即可定位。

## 5. PrepareCall 失败分类

```go
type PreparePhase int
const (
    PhaseTranslate PreparePhase = iota  // translator.TranslateRequest 失败
    PhaseQuirks                         // quirks.Rewrite 失败（vendor / 模型级 body 微调）
    PhaseBuild                          // adapter session BuildRequest / NewSession 失败
)

type PrepareError struct {
    Phase PreparePhase
    Err   error
}
```

- **PhaseTranslate**：`srcBody` 不符合 `SourceProtocol` 的 schema → `dispatch.ClassInvalid`
  → caller 应直接 abort 400（同请求换 endpoint 也会失败）
- **PhaseQuirks**：vendor / 模型级 body Rewriter 失败（详见 §4a）→ `dispatch.ClassInvalid`
  → caller 应直接 abort（同请求重试也会同样失败）
- **PhaseBuild**：vendor HTTP 构造失败（极少；通常是 endpoint 配置非法如 URL
  不可解析）→ `dispatch.ClassPermanent`

`invoker.Sender.Send` 用 `errors.As(*PrepareError)` 分流到不同 `Outcome.Class`
和返回值；wiring 层把两种都标 `Verdict.Stage = StagePrepare`，让 Policy 跟
"上游调用失败" 区分开。

## 6. Lookup：动态组合

```go
type Lookup interface {
    Get(ep *domain.Endpoint, srcProto domain.Protocol) Handler
}

type DefaultLookup struct{}

func (DefaultLookup) Get(ep *Endpoint, src Protocol) Handler {
    if ep == nil || ep.Protocol == ProtoUnknown {
        return nil
    }
    ad := adapter.Get(ep.Vendor)
    if ad == nil {
        return nil
    }
    tr := translator.Find(src, ep.Protocol)
    if tr == nil {
        return nil
    }
    return Combine(ad, tr)
}
```

**请求级注入**：M3 Envelope 给 `rc.Handlers` 填默认值 `DefaultLookup{}`；
多租户 / 灰度场景下后续 middleware（如 M2 Auth）可按 tenant 覆盖
`rc.Handlers` 走自定义 Lookup 实现（限定可用 vendor / 自定义 translator
chain）。

dispatcher / invoker / eligibility 一律走 `dispatch.HandlersFrom(rc)` 取
typed Lookup，不直接消费 adapter / translator registry。

## 7. `pkg/adapter` — vendor HTTP 层（facade 内部细节）

```go
type Metadata struct {
    Vendor              string            // openai|anthropic|gemini|ark|...
    SupportedModalities []domain.Modality // chat|embedding|image|...
}

type Factory interface {
    Metadata() Metadata
    NewSession(ctx, ep, env) (Session, error)
}

type Session interface {
    BuildRequest(body []byte) (*http.Request, error) // body = translator 已翻译过的字节
    Close() error
}

type Classifier interface {  // 可选
    Classify(status int, body []byte) *domain.AdapterError
}
```

**adapter 不再声明 NativeProtocol**——v0.5 把它写在 Metadata 上做 vendor 默认
协议，v0.6 删除，协议归属转移到 `Endpoint.Protocol`。

`Classifier` 实现自动透出到 `protocol.Handler` 接口：vendor adapter 实现 Classifier
时，`Combine(ad, tr)` 出来的 Handler 自动满足 `protocol.Classifier`，invoker 在
HTTP 非 2xx 时 type-assert 调用。

vendor 子包：

- `pkg/protocol/openai/` — vendor=openai + alias=ark
- `pkg/protocol/anthropic/`
- `pkg/protocol/gemini/`

每个 vendor 子包的 `init()` 只调 `adapter.Register("<vendor>", Factory{})`；
Handler 由 `DefaultLookup` 在请求时动态合成，不在 init() 注册矩阵。

## 8. `pkg/translator` — body shape 层（facade 内部细节）

```go
type Translator interface {
    Source() domain.Protocol // 接受的客户端协议
    Target() domain.Protocol // 翻译到的上游协议（match Endpoint.Protocol）

    TranslateRequest(srcBody []byte) ([]byte, error)
    NewResponseHandler() ResponseHandler
}

type ResponseHandler interface {
    Feed(chunk []byte) (clientBytes []byte, err error)
    Flush() (clientBytes []byte, usage *domain.Usage, err error)
}
```

**注册**：每个 translator 子包 `init()` 调 `translator.Register(...)`；启动期
全局表填好；`translator.Find(src, tgt)` 运行期查询。`DefaultLookup.Get` 据此
动态拿 translator。

内置 translator：

| src → tgt | 包 | 用途 |
|---|---|---|
| OpenAI → OpenAI | `translator/identity` | identity 透传（注 stream_options.include_usage） |
| Anthropic → Anthropic | `translator/identity` | identity 透传 |
| Responses → Responses | `translator/identity` | identity 透传 |
| OpenAI → Anthropic | `translator/openai_anthropic` | 客户端 OpenAI SDK → Anthropic 上游 |
| Anthropic → OpenAI | `translator/anthropic_openai` | 客户端 Anthropic SDK → OpenAI 上游 |
| OpenAI → Gemini | `translator/openai_gemini` | 客户端 OpenAI SDK → Gemini 上游 |
| Responses → OpenAI | `translator/responses_openai` | Responses 入口接到 Chat Completions endpoint |

**不要求补齐所有组合**。优先级：

1. 同协议 identity：客户端协议和 `ep.Protocol` 一致，尽量透传。
2. 已明确业务需求的跨协议组合：上表中列出的。
3. 未注册的组合 → `DefaultLookup.Get` 返 nil → eligibility 剔除 → 该 endpoint
   不参与本次请求。

## 9. eligibility 过滤

`pkg/selector/eligibility.Filter` 按 endpoint 候选 + `protocol.Lookup` 单一参数
过滤：

```go
for ep in candidates {
    h := handlers.Get(ep, env.SourceProtocol)
    if h == nil:
        removed (handler_missing)
    if !contains(h.Capabilities().SupportedModalities, env.Modality):
        removed (modality_unsupported)
    eligible
}
```

v0.5 的两次 lookup（AdapterLookup + TranslatorLookup）+ match check 合并到一次
Handler 查找。

## 10. invoker 流程

```go
func (s *Sender) Send(ctx, ep, env, srcBody, handler) (Outcome, error) {
    fire hook(ClientRequest)
    
    if handler == nil {
        return Outcome{Stage: StagePrepare, Class: ClassPermanent}
    }
    
    call, err := handler.PrepareCall(ctx, ep, srcBody)
    if err != nil {
        return Outcome{Stage: StagePrepare, Class: <PhaseTranslate→Invalid | PhaseBuild→Permanent>}
    }
    
    fire hook(UpstreamRequest, call.UpstreamBody)
    
    resp := client.Do(call.Request)
    class := classifyHTTPStatus(resp.StatusCode)
    if h, ok := handler.(protocol.Classifier); ok {
        class = h.Classify(resp.StatusCode, peekBody(resp))  // 细化
    }
    
    return Outcome{Stage: StageInvoke, Response: resp, Handler: handler, Class: class}
}

func (s *Sender) Forward(ctx, w, ep, resp, stream protocol.ResponseStream) ForwardResult {
    for chunk := resp.Body.Read():
        out := stream.Feed(chunk)
        w.Write(out); flush
    final := stream.Flush()
    w.Write(final)
}
```

`Outcome` 不再带 `Translator` 字段；改为 `Handler`，caller 用
`outcome.Handler.NewResponseStream()` 拿 ResponseStream 传给 Forward。

## 11. dispatch.Verdict.Stage

```go
type Stage int
const (
    StageInvoke   Stage = iota // HTTP 调用（默认）
    StageSelect               // 选 endpoint 失败
    StagePrepare              // 协议转换 / HTTP 构造失败
    StageReserve              // ratelimit 前扣失败
)
```

Policy.Decide 可以按 Stage 做更细粒度的决策——例：StagePrepare 失败说明
`ep.Protocol` 跟 srcProto 不匹配；继续 retry 同 endpoint 没意义，可以直接
Switch 到下个 model 或 Abort。

## 12. 新增 vendor / endpoint 步骤

1. 在 `pkg/protocol/<vendor>/` 实现 `adapter.Factory` 和 `adapter.Session`。
2. `init()` 中 `adapter.Register("<vendor>", Factory{})`。
3. 如果客户端要走的协议跟 vendor 的上游协议不一致，且 `pkg/translator/<src>_<dst>/`
   还没注册——新增 translator 实现并 `init()` 注册。
4. 在 `cmd/gateway/main.go` 加 blank import：
   - `_ "github.com/zereker/llm-gateway/pkg/protocol/<vendor>"`
   - `_ "github.com/zereker/llm-gateway/pkg/translator/<pair>"`（identity 已默认导入）
5. 重新构建并重启 gateway 进程。
6. deployer SQL INSERT 创建 endpoint：`vendor` 必须与注册名一致；`protocol` 必填，声明该
   endpoint 上游说什么协议。

## 13. 演进规则

- 协议归属永远是 endpoint 级；不要在 vendor adapter 上恢复 NativeProtocol。
- 不在 init() 静态注册 (vendor, srcProto) Handler 矩阵——保留运行期动态组合，
  让 rc.Handlers 覆盖能影响所有路径。
- 同一 vendor 多协议能力 → 多条 endpoint 行，分别设 Protocol。
- 不为"矩阵完整性"新增 translator；只在业务需要且没有 endpoint 走原生协议时
  才新增。
- 不把协议转换逻辑回塞 adapter——adapter 始终只管 HTTP 层。
- 新增 translator 必须覆盖请求转换、响应 handler、usage 提取和错误路径测试。
- 不恢复全局 canonical request，除非有明确消费者和字段保真策略。
