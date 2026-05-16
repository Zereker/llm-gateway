# 02 — Protocol Translation

本文记录协议转换边界：`pkg/adapter` 只处理厂商 HTTP 细节，`pkg/translator`
处理请求/响应 shape 转换和 usage 提取，`pkg/upstream` 把两者串起来。

核心原则：保留协议转换扩展能力，但不追求补齐任意 `source × target` 矩阵。
一个 vendor / endpoint 如果原生支持某个协议，就按该原生协议接入；不支持时只有在已存在
明确 translator 的情况下才转换，否则放弃该 endpoint。

## 1. 目标边界

```text
Client request
  -> M3 RequestEnvelope(raw body + source protocol + modality)
  -> M7 Schedule 选 endpoint
  -> upstream.Sender.Send
       -> adapter.Factory.NewSession(endpoint, envelope)
       -> translator.Get(source protocol, endpoint native protocol, modality)
       -> translator.TranslateRequest(raw body)
       -> adapter.Session.BuildRequest(translated body)
       -> http.Client.Do
  -> upstream.Sender.Forward
       -> translator.ResponseHandler.Feed chunks
       -> write client response
       -> finalize Usage
```

## 2. `RequestEnvelope`

M3 不再产出 `CanonicalRequest`。当前 envelope 是轻量路由信封：

```go
type RequestEnvelope struct {
    RawBytes       []byte
    Model          string
    SourceProtocol domain.Protocol
    Modality       domain.Modality
}
```

协议细节全部保留在 `RawBytes` 中，translator 直接基于 raw body 做转换。这样避免内部 canonical schema 覆盖不全导致字段丢失。

## 3. 协议能力归属

协议能力应尽量归属到 endpoint，而不是只归属到 vendor。

原因是同一个 vendor 可能同时提供多种原生协议。例如 Azure OpenAI 可以同时有
Chat Completions endpoint 和 Responses endpoint；这时应该建两类 endpoint：

- `native_protocol = openai`：接 `/chat/completions`。
- `native_protocol = responses`：接 `/responses`。

调度选择 endpoint 时应满足：

1. endpoint 支持当前请求的 modality。
2. endpoint 的 `native_protocol` 等于客户端 `source_protocol`，或存在已注册 translator。
3. 不存在原生协议或 translator 时，该 endpoint 不参与本次请求。

这条规则避免为了“理论兼容”去实现没有业务需求的协议互转。例如 vendor 只有
Chat Completions 能力时，不需要强行支持 Responses；vendor 同时具备 Chat 和
Responses 能力时，通过 endpoint 声明分别支持即可。

`native_protocol` 必须来自 endpoint 配置或 endpoint capabilities。admin 创建 endpoint 时必须校验该字段非空；缺失时 endpoint 不能保存。adapter 可以提供默认 metadata 作为文档/测试辅助，但调度和 translator lookup 不得 fallback 到 vendor/factory 级静态值。

## 4. Adapter：slim HTTP 层

`pkg/adapter/factory.go` 目标契约：

```go
type Metadata struct {
    Vendor              string
    NativeProtocol      domain.Protocol
    SupportedModalities []domain.Modality
}

type Factory interface {
    Metadata() Metadata
    NewSession(c context.Context, ep *domain.Endpoint, env *domain.RequestEnvelope) (Session, error)
}

type Session interface {
    BuildRequest(body []byte) (*http.Request, error)
    Close() error
}
```

adapter 负责：

- vendor 名和默认 native protocol 声明。
- endpoint auth/routing/extra 解析。
- 上游 URL、HTTP method、认证 header、content type、厂商特定 header。
- session 生命周期清理。

adapter 不负责：

- 客户端协议到上游协议的 JSON shape 转换。
- SSE / chunk 响应解析。
- usage 提取。
- retry/cooldown/endpoint 选择。
- 决定一个 endpoint 是否应该被强行跨协议转换。

## 5. Registry

adapter 通过 `init()` 注册：

```go
func Register(vendor string, f Factory)
func Get(vendor string) Factory
func Vendors() []string
```

注册表运行期无锁读，约定所有 `Register` 都发生在 init 阶段。重复 vendor 直接 panic。

因此新增 vendor adapter 是构建期能力变更：需要新增包、在 `cmd/gateway` 中 blank import，并重启 gateway 进程。网关不支持运行期动态注册 adapter。

`cmd/gateway` 目标 blank import：

- `pkg/adapter/openai`
- `pkg/adapter/anthropic`
- `pkg/adapter/gemini`

## 6. Translator

translator 是协议 shape 层，按 source protocol、target native protocol 和 modality
选择。translator 是显式能力，不是兜底机制：没有注册对应 translator 就说明该转换不支持。

目标内置 translator：

- `translator/identity`：同协议透传，覆盖 OpenAI、Anthropic、Responses 等 identity 场景。
- `translator/openai_anthropic`
- `translator/anthropic_openai`
- `translator/openai_gemini`

Gemini 当前是上游协议支持：客户端仍从 OpenAI/Anthropic/Responses 路由进入，M7 根据 endpoint vendor/native protocol 找到 translator 后转给 Gemini 上游。

不要求补齐所有组合。优先级是：

1. 同协议 identity：客户端协议和 endpoint native protocol 一致，尽量透传。
2. 明确需要的跨协议 translator：例如 OpenAI Chat → Anthropic、OpenAI Chat → Gemini。
3. 未注册的组合直接视为不支持，不作为调度候选或返回明确错误。

## 7. upstream.Sender

`pkg/upstream` 封装一次上游调用和响应转发：

```go
type Sender struct {
    lookup FactoryLookup
    client HTTPDoer
}

func (s *Sender) Send(ctx context.Context, ep *domain.Endpoint, env *domain.RequestEnvelope, raw []byte) (Outcome, error)
func (s *Sender) Forward(ctx context.Context, w http.ResponseWriter, ep *domain.Endpoint, resp *http.Response, h translator.ResponseHandler) ForwardResult
```

`Outcome` 包含 HTTP response、调度错误分类、HTTP status、reason、latency 和 translator。M7 将 `Outcome.ToScheduleResult()` 反馈给 scheduler。

请求体转换失败返回 `upstream.ErrInvalidRequest`，M7 直接 400，不重试其它 endpoint。

## 8. 协议与模态

`domain.Protocol` / `domain.Modality` 是路由、endpoint 能力、adapter metadata 和
translator lookup 的共同语言。当前路由侧只暴露项目已注册的 `/v1/...` 入口；
新增客户端协议需要同时补：

1. router 路由和 `WithSourceProtocol` 标记。
2. M3 对该协议顶层 `model` 字段的提取逻辑。
3. translator 注册。
4. 对应测试。

## 9. 新增 vendor / endpoint 步骤

1. 在 `pkg/adapter/<vendor>` 实现 `Factory` 和 `Session`。
2. `init()` 中调用 `adapter.Register("<vendor>", factory)`。
3. 在 endpoint 配置里声明该 endpoint 的 native protocol / modality 能力。
4. 如果 native protocol 不是客户端源协议，且业务确实需要跨协议调用，新增或复用 `pkg/translator/<src>_<dst>`。
5. 如果 vendor 已经原生支持目标客户端协议，优先新增对应 native endpoint，而不是新增 translator。
6. 在 `cmd/gateway/main.go` 加 blank import。
7. 重新构建并重启 gateway 进程。
8. 在 admin 侧创建 endpoint，`endpoints.vendor` 必须与注册名一致。

## 10. 演进规则

- 不把协议转换逻辑回塞 adapter。
- 不恢复全局 canonical request，除非有明确消费者和字段保真策略。
- 新增 translator 时必须覆盖请求转换、响应 handler、usage 提取和错误路径测试。
- 不为“矩阵完整性”新增 translator；只有业务需要且目标 vendor 没有原生协议能力时才新增。
- 同一 vendor 多协议能力应优先通过多个 endpoint/native protocol 表达。
