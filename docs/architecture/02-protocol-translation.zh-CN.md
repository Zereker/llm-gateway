[English](02-protocol-translation.md) | [简体中文](02-protocol-translation.zh-CN.md)

# 02 — 协议转换

本文档记录了协议转换的抽象和组成：client
协议输入、上游协议输出（预调用）、上游响应返回、客户端协议
出（呼叫后）。两期被包裹在`internal/protocol.Handler`门面下；
在内部它仍然由两个独立的子抽象组成 - `internal/protocol`
（供应商 HTTP 层）+ `internal/translator`（请求体结构层）——但消费者只能看到
处理器。

核心原则：

- 协议所有权是**端点级**属性（`Endpoint.Protocol`），而不是
  供应商一级。
- Handler是（endpoint，sourceProtocol）元组的端到端处理器；是的
  **根据请求动态组合**，而不是静态注册到矩阵中
  启动。
- 我们的目的不是填写任意 `source × target` 协议矩阵 -
  未注册的组合将被简单地视为不受支持；资格过滤
  删除该端点，请求要么回退，要么返回 503。

## 1. 抽象关系

```text
┌──────────────────────────────────────────────────────────────────┐
│ internal/protocol.Handler  (facade, consumers only see this)           │
│                                                                  │
│   ┌──────────────────────────┐  ┌────────────────────────────┐   │
│   │ internal/protocol.Factory      │  │ internal/translator.Translator  │   │
│   │ (vendor HTTP layer)      │  │ (body shape conversion +    │   │
│   │  - Metadata              │  │  usage)                     │   │
│   │  - NewSession            │  │  - Source / Target          │   │
│   │  - Session.BuildRequest  │  │  - TranslateRequest          │   │
│   │                          │  │  - NewResponseHandler       │   │
│   └──────────────────────────┘  └────────────────────────────┘   │
└──────────────────────────────────────────────────────────────────┘
                 ▲
                 │ Combine(ad, tr) → Handler
                 │
        DefaultLookup.Get(ep, srcProto) composes dynamically at request time
```

## 2. 端到端请求管道

```text
Client request
  ↓
M3 Envelope: writes rc.Envelope (RawBytes / SourceProtocol / Modality)
            + rc.Handlers = the built-in protocol.Lookup (internal/builtin.NewLookup)
  ↓
M5 ModelService: resolves model + fallback chain
  ↓
M7 Schedule → dispatch.Dispatcher.Dispatch(ctx, w, rc):
  loop {
    ep := Selector.Select(query)                                    // StageSelect
    handler := rc.Handlers.Get(ep, env.SourceProtocol)               // dynamically composed Handler
    if handler == nil { record StagePrepare; retry / fallback }

    invocation := InvokerFactory.For(ep, env, body, handler)
    res := invocation.Invoke(ctx)
      └─ reserve quota                                              // StageReserve
      └─ handler.PrepareCall(ep, srcBody) → Call{Request, UpstreamBody}  // StagePrepare
      └─ client.Do(req)                                             // StageInvoke

    if success: res.StreamTo(ctx, w)
      └─ handler.NewResponseStream().Feed/Flush — translates back to client protocol chunk-by-chunk
  }
```

## 3.`domain.Endpoint.Protocol`

**必填字段**。当部署者创建端点（SQL INSERT）时，它必须
显式声明该端点的上游使用哪个协议
(`openai` / `anthropic` / `gemini` / `responses` / ...);如果缺失或
`ProtoUnknown`、`DefaultLookup.Get` 返回 nil，并且资格会删除该端点。

```go
type Endpoint struct {
    ...
    Vendor   string             // openai|anthropic|gemini|ark|... — vendor adapter selection
    Protocol domain.Protocol    // openai|anthropic|gemini|responses|... — protocol ownership
    ...
}
```

**为什么协议是端点级而不是供应商级**：同一供应商可以托管
同时使用不同协议的多个端点。示例：

|供应商 |端点.协议 |需要转换 |
|---|---|---|
| anthropic | anthropic | (Anthropic → Anthropic) identity |
| anthropic | openai | (OpenAI → Anthropic) cross |
| openai | openai | (OpenAI → OpenAI) identity |
| openai | responses | (OpenAI → Responses)——不适用，实际方向相反 |
| openai | anthropic | (Anthropic → Anthropic，仅当供应商运行 Anthropic 兼容 API 时) |

供应商适配器不再声明 `NativeProtocol` - 它只知道 HTTP 层
详细信息（身份验证标头/URL/TLS）；协议所有权留给端点。

## 4. `internal/protocol.Handler` — 外观

```go
type Handler interface {
    Capabilities() Capabilities

    // pre-call: translate srcBody + wrap in vendor HTTP envelope
    PrepareCall(ctx, ep, srcBody) (*Call, error)

    // post-call: translate the response back to the client chunk-by-chunk
    NewResponseStream() ResponseStream
}

type Call struct {
    Request      *http.Request  // HTTP request ready to send to upstream
    UpstreamBody []byte         // translated byte copy (for audit/hook)
}

type Capabilities struct {
    SourceProtocol      domain.Protocol   // = translator.Source()
    UpstreamProtocol    domain.Protocol   // = translator.Target() == ep.Protocol
    SupportedModalities []domain.Modality // = protocol.Metadata().SupportedModalities
}
```

**功能不包含供应商** - 供应商是端点的属性，而不是端点的属性
处理器； Handler 是一个动态的（适配器、转换器）组合，并且它只
触及作为 `PrepareCall` 参数传递的特定端点。

## 4a. Quirks——端点级请求调整 (`internal/protocol/quirks`)

`translator`只负责从“客户端协议→上游”的结构转换
协议”；在同一个上游协议内，不同的供应商/型号仍然可以有
细微的差别。 **所有Quirks都是部署知识，存储在
`endpoints.quirks` JSON 列，由部署者直接通过 SQL 配置** — 否
供应商规则是硬编码在组合层中的。

两类典型的差异：

**请求体字段**
- OpenAI o1/o3/o4推理模型：`max_tokens` → `max_completion_tokens`；条带
  `temperature` / `top_p` / `presence_penalty` / `frequency_penalty`
- DeepSeek `deepseek-reasoner`：类似的限制
- Anthropic Claude 3.7+ 扩展思考：插入 `thinking` 块，并强制
  `temperature=1`
- vLLM / Ollama：剥离某些特定于 OpenAI 的字段

**标题字段**
- 不同的供应商使用不同的trace-id标头名称（`X-Request-Id` /
  `X-Ark-Request-Id` / `x-ds-request-id`等）——网关统一使用
  `X-Request-Id`，部署者配置重命名，以便上游接收
  它识别的标头名称
- 供应商私有标头（例如 `X-API-Version`）在端点上进行硬配置

插入转换器和适配器之间 - 主体和标头都贯穿其中
在交给适配器进行最终组装之前经过一轮：

```text
client body
  → translator.TranslateRequest          (client protocol → upstream protocol shape)
  → ep.Quirks.RewriteBody  + RewriteHeader  ← 4a (both segments run in one pass)
  → protocol.Session.BuildRequest(body, headers)   (HTTP envelope + merge quirks headers)
  → upstream
```

**适配器合并规则**：将怪异标头复制到 req.Header 后，适配器然后
编写自己的协议所需的标头（Auth / Content-Type /供应商版本标头，
等）— **最后写入获胜**。这意味着：
- 部署者可以添加任意供应商私有标头（X-Vendor-Tag 等）
- 如果部署者错误地用其他内容覆盖授权，则不会
  中断请求（适配器将其覆盖作为安全网）

**DSL**（存储在 `endpoints.quirks` JSON 列中）：

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

正文/标题子部分中的应用程序顺序是固定的：
`rename → strip → set → set_default`（先腾出空间，然后清理，然后覆盖，
然后最后填写默认值）。

接口（`internal/protocol/quirks/quirks.go`）：

```go
type Rewriter interface {
    RewriteBody(body []byte) ([]byte, error)
    RewriteHeader(h http.Header)
}

// Compiles endpoint.Quirks JSON → Rewriter; strict mode (typo'd fields error out immediately).
func CompileJSON(specJSON []byte) (Rewriter, error)
```

**combine.go 缓存**：仅编译相同的规范文字 (`string(ep.Quirks)`)
一次，并且生成的重写器在请求之间共享；不同的端点
相同的Quirks配置也分享一下。

**NULL 列/空 JSON / `{}`** → 无操作重写器，零开销。

**处理部署者错误配置**：如果规范 JSON 无法解析（或者有一个
未知字段拼写错误），对该端点的请求返回 `PhaseQuirks`
(`dispatch.ClassInvalid`)，调度程序直接中止而不重试。一个
配置错误的端点总是会出错，因此将其固定到指标/日志就足以
找到它。

## 5.PrepareCall失败分类

```go
type PreparePhase int
const (
    PhaseTranslate PreparePhase = iota  // translator.TranslateRequest failed
    PhaseQuirks                         // quirks.Rewrite failed (vendor / model-level body tweak)
    PhaseBuild                          // adapter session BuildRequest / NewSession failed
)

type PrepareError struct {
    Phase PreparePhase
    Err   error
}
```

- **PhaseTranslate**：`srcBody`与`SourceProtocol`架构不匹配→
  `dispatch.ClassInvalid` → 调用者应该直接用 400 中止（切换
  同一请求的端点将以同样的方式失败）
- **PhaseQuirks**：供应商/模型级主体重写器失败（有关详细信息，请参阅§4a）→
  `dispatch.ClassInvalid` → 调用者应该直接中止（重试相同的
  请求也会以同样的方式失败）
- **PhaseBuild**：供应商 HTTP 构建失败（罕见；通常是无效端点
  配置（例如无法解析的 URL）→ `dispatch.ClassPermanent`

`invoker.Sender.Send` 使用 `errors.As(*PrepareError)` 路由到不同的
`Outcome.Class`和返回值；布线层将两种情况标记为
`Verdict.Stage = StagePrepare`，因此Policy可以将它们与“上游调用
失败。”

## 6.查找：动态合成

```go
type Lookup interface {
    Get(ep *domain.Endpoint, srcProto domain.Protocol) Handler
}

type DefaultLookup struct {
    factories map[string]Factory
    translators *translator.Registry
}

func (l DefaultLookup) Get(ep *Endpoint, src Protocol) Handler {
    if ep == nil || ep.Protocol == ProtoUnknown {
        return nil
    }
    ad := l.factories[ep.Vendor]
    if ad == nil {
        return nil
    }
    // direct route preferred; on miss, fall back via pivot (OpenAI) composition, see §6a
    tr := l.translators.FindVia(src, ep.Protocol, ProtoOpenAI)
    if tr == nil {
        return nil
    }
    return Combine(ad, tr)   // cached inside this lookup instance
}
```

**请求级注入**：应用程序显式构造 `builtin.NewLookup()` 和
router通过Envelope注入；在多租户/金丝雀场景中，中间件
Auth) 可以使用自定义 Lookup 实现覆盖每个租户的 `rc.Handlers`
（限制可用供应商/自定义转换链）。

调度员/调用者/资格均通过`dispatch.HandlersFrom(rc)`来
获取类型化的查找，并且永远不会直接使用适配器/转换器注册表。

## 6a。缺失对的回退：主元组合（控制笛卡尔积）

在协议对矩阵的三个增长轴中，**供应商轴已经
已折叠**（协议属于端点级别+OpenAI兼容别名
共享工厂 + Quirks吸收供应商差异 - 添加新供应商的时间复杂度为 O(1) 并且
不进入矩阵）。剩下的，客户端协议 × 上游协议，是
轴增长缓慢，但随着更多协议的加入，它仍然呈倍数增长。
治理策略有两个层次：

**第 1 层：直接转换器（高保真度，首选）** — 对于每个 (src, tgt) 对
有真实流量的，手写一个`internal/translator/<src>_<tgt>/`，完全映射
特定于协议的字段（思维块/cache_control/工具架构等）。

**第 2 层：主元组合（回退，可能有损）** — `Registry.FindVia(src,
tgt,pivot)` attempts `Compose(Find(src,pivot),Find(pivot,tgt))`当直接
路由错过：

```text
Request direction: src body → front(src→openai) → openai body → back(openai→tgt) → tgt body
Response direction: tgt chunks → back.handler(tgt→openai) → openai body → front.handler(openai→src) → src body
```

- 枢轴固定为 **OpenAI 协议**（事实上的行业通用语言；
  每个现有的跨协议对的一端都已经有它，所以当加入一个
  新协议，首先使用 OpenAI 编写其转换对，以最大化组合
  自动覆盖）
- **直接路由始终优先于组合**：`FindVia` 检查
  直达路由优先；一旦为流行的对添加了直接实现，它
  自动接管，对调用者透明
- 用量提取优先采用上游侧处理器（最接近真实情况）
  回应；客户端看到的是二手枢轴字节）
- 组合处理器在创建时记录 `slog.Warn`（查找的处理器缓存保证
  每个 (vendor, src, tgt)) 只有一个警告 - **有损**：枢轴无法表达的字段是
  迷失在双跳中
- 如果任一腿缺失→返回零→资格像往常一样将其删除，相同
  行为如前

**当前覆盖范围**：7 个直接配对；成分自动填写
Anthropic→Gemini，反应→Anthropic，反应→Gemini（7/12 → 10/12）；的
剩余的两个 *→ 响应对无法组合，因为没有
`openai→responses` 转换器 - 应该在下面添加直接实现
一旦真正的需求出现，第一层。

**演进规律**：
- 频繁组合警告 (src, tgt) 对 = 添加直接信号的信号
  转换者
- **不要**直接跳转到构建规范的 IR（全局中间表示）
  因为组合可用 - 该项目已删除
  v0.5 中的 `Envelope.Canonical` 并且不会返回该路径：全保真 IR
  双跳是完全有损的+增加了流复杂性，这比
  “直接高保真+合成回落”的两层结构

### 6b。内容特征覆盖率和有损可观测性

跨协议对并不都包含每个请求功能。当前覆盖范围：

|一对|文字|工具调用|多式联运（图像）|供应商特定|
|---|---|---|---|---|
| `openai_anthropic` | ✅ | ✅ (`tools[].strict` 转入) | ✅ |扩展思维往返（通过`reasoning_content`/`reasoning_signature`）； `web_search_result_location` 引用 → `annotations[].url_citation`； document/search_result 引用和 `redacted_thinking`/`server_tool_use`/`mcp_tool_use` 块仍然下降（见下文）|
| `anthropic_openai` | ✅ | ✅ (`tools[].strict` 已转) | ✅ | — |
| `openai_gemini` | ✅ | ✅ | ✅ | `n`/`candidateCount`、`response_format`、Gemini3 `thoughtSignature` 往返|
| `openai_cohere` | ✅ | ✅ | ✅ | `command-a-reasoning-*` `thinking`区块→`reasoning_content`;引用量仍然下降（未确定与 OpenAI 兼容的结构）|
| `openai_bedrock` | ✅ | ✅ | ❌ | Bedrock **Converse** API（与模型无关；与旧的 InvokeModel 路径不同，旧的 InvokeModel 路径保留在 `openai_anthropic` 上，因为 InvokeModel 的主体已经是 Anthropic Messages JSON）。仅针对 Claude-on-Bedrock 流量进行验证。 `reasoningContent` → `reasoning_content`（仅响应端 - 签名尚未在下一轮请求时往返）； `citationsContent` 仍然掉落（未确定与 OpenAI 兼容的结构，与 Cohere 的引文相同）|

**工具调用**：请求端地图`tools` / `tool_choice`，辅助工具
OpenAI 之间的调用和工具结果 `tool_calls` + `role:"tool"`
模型和每个上游的原生结构（Anthropic `tool_use`/`tool_result`
块；Gemini `functionCall`/`functionResponse` 零件，其中 `args` 是
JSON **对象**而不是字符串 - 单一字段结构不对称性与
OpenAI/Anthropic/Cohere； Cohere v2 的结构与 OpenAI 的结构几乎相同）。
响应端映射非流式和流式，包括并行工具
来电；当消息发送时，Gemini 的 `finish_reason` 被覆盖为 `tool_calls`
携带它们，因为Gemini自己的 `finishReason` 通常只是 `STOP`。
`tool_choice` 保真度各不相同：Anthropic和Gemini可以强制一种特定的
命名工具（`{"type":"tool",...}` / `allowedFunctionNames`）；仅 Cohere v2
有 `REQUIRED`/`NONE`，因此命名函数选择回落到 `REQUIRED`
（强制*某些*调用，不一定是那个）。

**多模态（图像）**：所有四对都转换 OpenAI 的 `image_url` 内容
部分（`data:` URI 或纯 URL）。 Anthropic的`image`区块用途
`source.type` = `base64`/`url`;Gemini的部分使用`inlineData`
（`mimeType`+`data`）对于base64或`fileData`（`fileUri`）网址 — 两者
供应商自己获取纯 URL，因此网关永远不会代理图像
字节。 Cohere v2 的 `ImageContent`/`ImageUrl` 类型（根据
官方 cohere-python SDK）在结构上与 OpenAI 相同
`image_url` 部分，因此该对比
重塑。所有四对音频/视频/文档内容均未处理。

**扩展思维**（仅`openai_anthropic` - 另一个方向没有
它上游的概念，以及相同的协议 `identity/anthropic` 已经
逐字节传递它，无需转换）：Anthropic
`thinking` 块表面上的 OpenAI 形响应为
`message.reasoning_content`（匹配真实OpenAI兼容的字段名称
推理模型供应商已使用）加上 `message.reasoning_signature`
（Anthropic特定）。客户端将助理消息回显为
下一轮的历史记录往返两个字段，并且 `buildAssistantMessage`
重建该回合内容中的 Anthropic `thinking` 块 **first**
array — Anthropic 拒绝历史中没有前面的 `tool_use` 块
一旦启用扩展思维，就会签署思维块，因此重播
需要逐字签名（而不是重新生成），而不是装饰性的。

**引用**（仅`openai_anthropic`，响应端）：文本内容
块的 `citations` 数组仅转换为
`web_search_result_location` 引文类型，与其他引文类型不同
人为引用类型 — 带有 `url`/`title`，因此映射清晰
为 OpenAI 自己的 `message.annotations[].url_citation` 结构（已验证
官方 openai-python SDK 的类型定义）。它表面上
非流响应和流路径（作为一个 `annotations` delta
每个内容块的块，一旦该块在`content_block_stop`处发出
已知运行的 `content` 字符串中的字符范围 - OpenAI 自己的
流 API 没有记录增量引用增量，所以这是
整体发出而不是零碎发出）。所有其他引文类型
（`char_location`/`page_location`/`content_block_location`用于文档，
`search_result_location` 为 `search_result` 工具块）仅携带
`document_index` 没有 URL，因此它没有 OpenAI 兼容的表示
并被删除——与 Cohere 的引文处理方式相同（上文§）。

**已知差距，尚未实施**（`openai_anthropic`，通过真实发现
从 langchain-ai/langchain 的官方 langchain-anthropic 捕获流量
包，Apache 2.0，`libs/partners/anthropic/tests/cassettes/`）：
`redacted_thinking` 块（一个不透明、无签名的思维变体，
就像常规的 `thinking` — 必须逐字往返历史）是
通过 `reasoning_content` 默默地掉落而不是浮出水面；和Anthropic的
服务器执行的工具内容块（`server_tool_use` /
`web_search_tool_result` / `bash_code_execution_tool_result` / `mcp_tool_use`
/ `mcp_tool_result` / `tool_search_tool_result`）默默地落在
响应方及其工具*定义* (`{"type":"web_search_20250305",
...}`, `mcp_servers`, `code_execution`等）甚至无法配置
今天的 OpenAI 结构的请求（`translateRequest` 中的工具循环跳过
任何非 `"function"` 工具类型）。是否/如何暴露 Anthropic 的
通过 OpenAI 客户端协议的服务器端工具是产品决定的，
不是实地测绘工作，并且有意未实施
那个决定。

**连贯推理**（仅限 `openai_cohere`）：`command-a-reasoning-*` 模型
在之前发出 `{"type":"thinking","thinking":...}` 内容块
最终的 text/tool_calls 块 — Cohere 的扩展思维模拟，已验证
针对真实捕获的 `command-a-reasoning-08-2025` 工具调用响应
（参见 `internal/cassette/testdata/fieldmatrix/upstream/README.zh-CN.md`）。它
表面与 Anthropic 的表面相同，如 `message.reasoning_content`
（非流）或 `reasoning_content` 按内容索引键控的增量块
（流式传输，因为 `content-delta` 事件仅重复更改的字段 —
`.thinking` 或 `.text` — 不是从该索引跟踪的类型
之前的 `content-start` 事件）。与 Anthropic 不同，Cohere 的思维块
不携带签名，并且请求方没有入站字段，因此 -
匹配 Vercel AI SDK 自己的 Cohere 提供商 — 它不会被发回
历史回放；丢弃它不会造成任何损失，因为没有签名
如果没有，链 Cohere 将拒绝后续的 `tool_calls` 消息。

**Gemini 3 `thoughtSignature`** (`openai_gemini`)：Gemini 的每次呼叫模拟
Anthropic 的思维签名 — 一个不透明的签名斑点，作为
`functionCall` 部分的同级字段。真实捕获的Gemini 3值和
其出处保存在 `internal/translator/openai_gemini/translator_test.go` 中；
原始盒式磁带是 opencassette 的 `Vendored()` 语料库的一部分。
在 OpenAI 结构的响应中显示为 `tool_calls[].thought_signature`
并在该工具调用回显时重播到同一部分
历史。与 Anthropic 的单一思维块不同，这是每次调用，所以它
单独调用每个工具，而不需要消息级别
字段 — 具有并行工具调用的请求保留每个调用自己的签名。

**有损可观测性**：无论一对仍然掉落，都不能默默掉落（
与上面的主元组合警告相同的规则）。每对呼叫
`translator.ReportLossyRequest(src, tgt, body, only...)` 位于顶部
`TranslateRequest`； `only` 将报告限制为仍配对的功能
丢弃（一对已经实现了某个功能的对象停止传递其标签；
没有什么可报告的配对根本就没有调用它 - 请参阅
`anthropic_openai`/`openai_anthropic`，截至表中均已完全覆盖
上面）。它：

- 增量 `llm_gateway_translator_feature_dropped_total{src,tgt,feature}`
  (`feature` = `tools | tool_calls | multimodal`) 对于每个删除请求，以及
- 记录一次性 `slog.Warn`（src、tgt、feature） — 客户端发送图像
  每个请求都会产生一个警告，而不是每个请求都会产生一个警告。

通过 gjson 尽最大努力进行检测，并且不会改变主体。身份（相同
协议）转换器将所有内容都通过但不调用它。一个上升的
`feature_dropped_total` 为一对，是实现真正转换的信号
对于那里的那个功能。

## 7. `internal/protocol` — 供应商 HTTP 层（外观的内部细节）

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
    BuildRequest(body []byte) (*http.Request, error) // body = bytes already translated by translator
    Close() error
}

type Classifier interface {  // optional
    Classify(status int, body []byte) *domain.AdapterError
}
```

**适配器不再声明 NativeProtocol** — v0.5 将其放在元数据上作为
供应商默认协议，v0.6 删除了它，协议所有权移至
`Endpoint.Protocol`。

`Classifier`实现自动表面通过`protocol.Handler`
接口：当供应商适配器实现 Classifier 时，由
`Combine(ad, tr)` 自动满足 `protocol.Classifier`，并且调用者
type-asserts 并在非 2xx HTTP 响应上调用它。

供应商子包：

- `internal/protocol/openai/` — 供应商=openai + 别名=ark
- `internal/protocol/anthropic/`
- `internal/protocol/gemini/`
- `internal/protocol/cohere/`
- `internal/protocol/azureopenai/` — 供应商=azure-openai;线路协议是OpenAI自己的（`ep.Protocol: openai`），只有HTTP层（URL结构+`api-key`标头）不同
- `internal/protocol/bedrock/` — 供应商=Bedrock；一个工厂，由 `ep.Protocol` 选择的两个会话：`anthropic`（仅限 InvokeModel，Claude-on-Bedrock）或 `bedrock`8（Converse，与模型无关） — 参见§6b)

每个vendor子包只定义其`Factory`类型； `internal/builtin.NewLookup`
在启动时组装工厂映射（由供应商名称键入），并且处理器是
由 `DefaultLookup` 在请求时动态合成，未注册到
矩阵。

## 8. `internal/translator`——请求体结构层（门面的内部实现）

```go
type Translator interface {
    Source() domain.Protocol // client protocol accepted
    Target() domain.Protocol // upstream protocol translated to (matches Endpoint.Protocol)

    TranslateRequest(srcBody []byte) ([]byte, error)
    NewResponseHandler() ResponseHandler
}

type ResponseHandler interface {
    Feed(chunk []byte) (clientBytes []byte, err error)
    Flush() (clientBytes []byte, usage *domain.Usage, err error)
}
```

**注册**：`internal/builtin.NewLookup` 建一 `translator.Registry`
（通过 `translator.NewRegistry(...)`）在启动时来自每个转换器子包；
`Registry.Find(src, tgt)` 在运行时查询。 `DefaultLookup.Get` 使用此
动态获取转换器。

内置转换器：

|源 → 目标 |包|目的|
|---|---|---|
| OpenAI → OpenAI | `translator/identity` |身份传递（注入stream_options.include_usage）|
| Anthropic → Anthropic | `translator/identity` | identity 直通 |
| Responses → Responses | `translator/identity` | identity 直通 |
| OpenAI → Anthropic | `translator/openai_anthropic` |客户端 OpenAI SDK → Anthropic 上游 |
| Anthropic → OpenAI | `translator/anthropic_openai` |客户端 Anthropic SDK → OpenAI 上游 |
| OpenAI → Gemini | `translator/openai_gemini` |客户端 OpenAI SDK → Gemini 上游 |
| OpenAI → Cohere | `translator/openai_cohere` |客户端 OpenAI SDK → Cohere v2/聊天上游 |
| OpenAI → Bedrock | `translator/openai_bedrock` |客户端 OpenAI SDK → Bedrock **Converse** 上游（`ep.Protocol: bedrock`；与 `ep.Protocol: anthropic` InvokeModel 路径不同，该路径重用 `openai_anthropic`）|
| Responses → OpenAI | `translator/responses_openai` |将 Responses 入口接到 Chat Completions 端点 |

**我们不要求填写每个组合**。优先级：

1、同协议身份：当客户端协议匹配`ep.Protocol`时，通过
   尽可能多的。
2. 具有明确、既定业务需求的跨协议组合：
   上表。
3.未注册的组合→`DefaultLookup.Get`返回nil→资格将其删除
   → 该端点不参与此请求。

## 9. 资格过滤

内部助手 `internal/dispatch.filterEligible` 使用
单个 `protocol.Lookup` 参数：

```go
for ep in candidates {
    h := handlers.Get(ep, env.SourceProtocol)
    if h == nil:
        removed (handler_missing)
    if !endpointSupportsModality(ep.Capabilities.Modalities, h.Capabilities().SupportedModalities, env.Modality):
        removed (modality_unsupported)
    eligible
}
```

端点上的 `Capabilities.Modalities` 是端点级别白名单，只能
缩小供应商的 `SupportedModalities`，切勿扩大它；为准确的交叉点
语义参见[03 §3](./03-endpoint-scheduling.zh-CN.md#3-candidate-eligibility-filtering)。

旧的 v0.5 结构进行了两次查找（供应商/转换者）以及匹配检查；截至
v0.6+ 这已合并到单个处理器查找中。

## 10. 调用者流程

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
        class = h.Classify(resp.StatusCode, peekBody(resp))  // refine
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

`Outcome` 不再携带 `Translator` 字段；它已替换为 `Handler`，
调用者使用`outcome.Handler.NewResponseStream()`来获取ResponseStream
传递给转发。

## 11.dispatch.Verdict.Stage

```go
type Stage int
const (
    StageInvoke   Stage = iota // HTTP call (default)
    StageSelect               // endpoint selection failed
    StagePrepare              // protocol translation / HTTP construction failed
    StageReserve              // ratelimit pre-deduction failed
)
```

Policy.Decide 可以根据 Stage 做出更细粒度的决策 - 例如，
StagePrepare失败意味着`ep.Protocol`与srcProto不匹配；没有意义
重试相同的端点，因此可以直接切换到下一个模型或中止。

## 12. 添加新供应商/端点的步骤

**OpenAI 兼容供应商（快速路径，无需重建）**：将名称添加到
gateway.yaml 中的 `vendors.openai_compatible` （和 console.yaml，因此控制
平面的端点验证器也接受它），重新启动，然后创建端点
行为 `protocol: openai`。编译的快速路径 (`openai.Aliases()`)
保留值得与二进制文件一起发布的名称。

**拥有自己的有线协议的供应商**：

1、在`internal/protocol/<vendor>/`中实现`protocol.Factory`和`protocol.Session`。
2.导出工厂并添加到`internal/builtin.NewLookup`。
3. 如果客户端使用的协议与供应商的上游协议不匹配，
   并且 `internal/translator/<src>_<dst>/` 尚未注册 — 添加新转换
   使用导出的 `New()` 构造函数实现。
4. 将该转换器实例添加到 `internal/builtin.NewLookup` 中的显式列表中。
5. 重建并重新启动网关进程。
6. 部署者通过 SQL INSERT 创建端点：`vendor` 必须与注册的端点匹配
   姓名； `protocol` 是必需的，并声明该协议的上游协议
   端点说话。

## 13.演进规则

- 协议所有权始终是端点级别的；不要恢复 NativeProtocol
  供应商适配器。
- 不要在启动时静态注册（供应商，srcProto）处理器矩阵 - 保留
  运行时动态组合，以便重写 rc.Handlers 可以影响所有路径。
- 同一供应商的多个协议功能→多个端点行，每个端点行
  有自己的协议集。
- 不要仅仅为了“矩阵完整性”而添加转换器——只有在存在时才添加转换器
  真正的业务需求，并且没有端点运行本机协议。
- 不要将协议转换逻辑推回到适配器中——适配器始终是
  只负责HTTP层。
- 新转换人员必须具有请求转换、响应处理器的测试覆盖率，
  使用提取和错误路径。
- 不要恢复全局规范请求，除非有明确的消费者和
  现场保真策略。
