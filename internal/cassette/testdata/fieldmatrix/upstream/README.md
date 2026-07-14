# 真实上游响应样本

用真实厂商上游实测抓取的响应体（已脱敏：去除供应商标识、密钥、签名 URL，model 名替换为中性占位），供 E2E / 单测回放，避免每次测试都要真实调用模型。样本的价值在于覆盖各厂商的**字段结构变体**，与具体是哪家供应商无关。

> 这里的文件都是从第三方开源项目的真实 VCR cassette **精加工**出来的（截断/脱敏/改名，适配我们的 E2E 测试）。如果要查某个字段的**原始、未加工**真实形状,去 opencassette 模块的语料（`opencassette.Vendored()` 第三方原始 cassette / `opencassette.Corpus()` 我们自录）——那边存的是原始 cassette 全文,还有一批我们尚未实现的真实特征（citations、redacted_thinking、MCP、code execution 等）。

| 文件 | 协议 / 形态 | 结构要点 |
|---|---|---|
| `chat-openai-compat.json` | OpenAI Chat 非流 | 含 `reasoning_content` 扩展字段 |
| `chat-openai-compat-reasoning.json` | OpenAI Chat 非流 | 含 `matched_stop` / `reasoning_tokens` 等厂商扩展字段 |
| `messages-anthropic-compat-stream.sse` | Anthropic Messages 流 | **厂商变体**：`message_start` 里 `input_tokens=0`，完整 usage 在 `message_delta`（区别于官方在 `message_start`），是 extractor 兼容性回归的关键样本 |
| `responses-native.json` | OpenAI Responses 非流 | 原生 Responses 响应，含 `reasoning` 输出项与 `status` |
| `messages-anthropic-compat-thinking-stream.sse` | Anthropic Messages 流 | extended thinking：`thinking_delta` → `signature_delta`（真实签名），usage 含 `cache_creation`/`service_tier`/`inference_geo` |

同目录上一级的 `chat-full.json` / `responses-full.json` / `responses-text.json` 是配套的满参数**请求** fixtures。

重新采集：起真实上游 endpoint 后用 curl 抓取，流式加 `-N` 存 `.sse`；长流按 SSE 空行分帧截取头尾（保留 usage 所在的收尾事件）。**入库前务必脱敏**：去掉真实 model 名、供应商域名、密钥、签名 URL query。

## 来源与许可

`messages-anthropic-compat-thinking-stream.sse` 的数据来自 [simonw/llm-anthropic](https://github.com/simonw/llm-anthropic)（Apache License 2.0）的 `tests/cassettes/test_anthropic/test_stream_events_thinking.yaml` VCR cassette，经 gzip 解压 + 截断/脱敏后收录（原始 cassette 里没有 API key——pytest-recording 录制时已排除鉴权头）。相关测试用例见 `internal/translator/openai_anthropic/translator_test.go` 里的 `TestStreaming_Thinking` / `TestTranslateResponse_Thinking` / `TestTranslateRequest_ThinkingRoundTrip`（用真实数据内联在测试里,不是从本文件读取）。原始 cassette 全文（含 web search 等本目录已不再保留衍生样本的场景）在 opencassette 模块的 `Vendored()` 语料里。

同一个 cassette 仓库的 `test_image_prompt.yaml` 提供了一张真实的 base64 PNG，用在 `openai_anthropic`/`anthropic_openai`/`openai_gemini`/`openai_cohere` 四个包的 `TestTranslateRequest_Image*` 系列测试里验证多模态图片透传（内联在测试代码中，未单独存成 fixture 文件——它是一张通用图片，不是某个厂商专属数据，跨包复用没问题）。

Gemini 3 `thoughtSignature` 的真实数据（来自 [simonw/llm-gemini](https://github.com/simonw/llm-gemini)，Apache License 2.0）内联在 `internal/translator/openai_gemini/translator_test.go` 的 `TestTranslateResponse_ThoughtSignature` / `TestTranslateRequest_ThoughtSignatureRoundTrip` / `TestResponseHandler_SSE_ThoughtSignature` 里；原始 cassette（`test_tools_with_gemini_3_thought_signatures.yaml`）在 opencassette 模块的 `Vendored()` 语料里。

`internal/translator/openai_cohere` 的 tool calling / vision / reasoning 覆盖用真实数据核对，来自 [langchain-ai/langchain-cohere](https://github.com/langchain-ai/langchain-cohere)（MIT License）`libs/cohere/tests/integration_tests/cassettes/` 下的多个 VCR cassette：`test_invoke_tool_calls.yaml` / `test_streaming_tool_call.yaml`（v2/chat 的 tool_calls + 流式 tool-call-start/delta/end 事件形状）、`test_invoke_with_vision_base64.yaml`（image_url 透传）、`test_who_founded_cohere_with_custom_documents.yaml`（citations 的真实字段形状：`sources[].type=document`，无 URL，证实了它确实不能映射到 OpenAI `annotations[].url_citation`——这个设计决策仍待用户拍板，未写代码）、`test_command_a_reasoning_with_tool_call.yaml`（command-a-reasoning-08-2025 的 `{"type":"thinking","thinking":...}` 内容块，Cohere 版的 extended thinking，但不带签名，也不需要在历史里原样带回——这点额外用 Vercel AI SDK 的 Cohere provider 源码（`packages/cohere/src/convert-to-cohere-chat-prompt.ts`，同样丢弃 reasoning 不回传）交叉验证）。相关测试见 `internal/translator/openai_cohere/translator_test.go` 里的 `TestTranslateResponse_Thinking` / `TestCohereStreamTranslate_Thinking`（真实数据内联在测试里）。

`internal/translator/openai_anthropic` / `anthropic_openai` 的 `tools[].strict` 透传用真实数据核对，来自 [langchain-ai/langchain](https://github.com/langchain-ai/langchain)（Apache License 2.0）官方 `langchain-anthropic` 分包 `libs/partners/anthropic/tests/cassettes/test_strict_tool_use.yaml.gz`（gzip 压缩的 VCR cassette）——证实 Anthropic 的工具定义确实原样接受 `strict` 字段（与 OpenAI 同名），此前两个方向的转换都在解析时静默丢弃了它。相关测试见两个包 `translator_test.go` 里的 `TestTranslateRequest_ToolStrict`。

同一批 cassette 里的 `test_citations.yaml.gz` 证实了 Anthropic 的 `web_search_result_location` citation 类型带 `url`/`title`（其他类型——`char_location`/`page_location`/`content_block_location`/`search_result_location`——只有 `document_index`，没有 URL），据此在 `internal/translator/openai_anthropic` 里把前者映射到 OpenAI 的 `message.annotations[].url_citation`（非流式 + 流式都做了，流式在 `content_block_stop` 时整块发一次 annotations delta，因为 OpenAI 官方 API 没有"增量 citation"的文档化先例）；后者（无 URL）继续丢弃，和 Cohere citations 保持同一处理方式。相关测试见 `TestTranslateResponse_Citations` / `TestStreaming_Citations`（citations_delta/text_delta 的真实事件形状取自 test_citations.yaml.gz，`web_search_result_location` 的具体字段取自 Claude 官方文档的 web search 工具页面）。

同一批 cassette 里还发现了更大的、尚未实现的真实特征——`redacted_thinking` 多块顺序回传、`server_tool_use`/`web_search_tool_result`/`bash_code_execution_tool_result`/`mcp_tool_use`/`mcp_tool_result`/`tool_search_tool_result` 内容块目前整体被静默丢弃，且这些 Anthropic 原生服务端工具的**请求侧配置**（`{"type":"web_search_20250305",...}`、`mcp_servers`、`code_execution` 等）目前通过 OpenAI 协议根本传不进去（`translateRequest` 的 tools 循环只认 `"function"` 类型）——这些留作后续任务，未写代码：是否要把 Anthropic 服务端工具透传给 OpenAI 协议客户端是产品决策，不是字段映射问题，需要专门讨论怎么设计透传。
