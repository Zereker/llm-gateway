# 真实上游响应样本

用真实厂商上游实测抓取的响应体（已脱敏：去除供应商标识、密钥、签名 URL，model 名替换为中性占位），供 E2E / 单测回放，避免每次测试都要真实调用模型。样本的价值在于覆盖各厂商的**字段结构变体**，与具体是哪家供应商无关。

| 文件 | 协议 / 形态 | 结构要点 |
|---|---|---|
| `chat-openai-compat.json` | OpenAI Chat 非流 | 含 `reasoning_content` 扩展字段 |
| `chat-openai-compat-stream.sse` | OpenAI Chat 流 | SSE（截取头尾），保留 usage chunk 与 `[DONE]` |
| `chat-openai-compat-toolcall.json` | OpenAI Chat 非流 | `finish_reason=tool_calls` 的工具调用响应 |
| `chat-openai-compat-reasoning.json` | OpenAI Chat 非流 | 含 `matched_stop` / `reasoning_tokens` 等厂商扩展字段 |
| `messages-anthropic-compat.json` | Anthropic Messages 非流 | 含 `thinking` block + `tool_use` |
| `messages-anthropic-compat-stream.sse` | Anthropic Messages 流 | **厂商变体**：`message_start` 里 `input_tokens=0`，完整 usage 在 `message_delta`（区别于官方在 `message_start`），是 extractor 兼容性回归的关键样本 |
| `responses-native.json` | OpenAI Responses 非流 | 原生 Responses 响应，含 `reasoning` 输出项与 `status` |
| `responses-native-stream.sse` | OpenAI Responses 流 | SSE（截取），保留 `response.completed` 的嵌套 usage |
| `images-openai-compat.json` | Images 生成 | usage 用 `output_tokens` 字段族 + `generated_images` 扩展（URL 已脱敏） |
| `messages-anthropic-compat-thinking-stream.sse` | Anthropic Messages 流 | extended thinking：`thinking_delta` → `signature_delta`（真实签名），usage 含 `cache_creation`/`service_tier`/`inference_geo` |
| `messages-anthropic-compat-server-tool-use-stream.sse` | Anthropic Messages 流 | server-side 工具（web search）：`server_tool_use` block + `web_search_tool_result`（结果数组截断到 1 条，`encrypted_content` 已替换为占位），usage 含 `server_tool_use.web_search_requests` 计费维度 |

同目录上一级的 `chat-full.json` / `responses-full.json` / `responses-text.json` 是配套的满参数**请求** fixtures。

重新采集：起真实上游 endpoint 后用 curl 抓取，流式加 `-N` 存 `.sse`；长流按 SSE 空行分帧截取头尾（保留 usage 所在的收尾事件）。**入库前务必脱敏**：去掉真实 model 名、供应商域名、密钥、签名 URL query。

## 来源与许可

`messages-anthropic-compat-thinking-stream.sse` 和 `messages-anthropic-compat-server-tool-use-stream.sse` 两个文件的数据来自 [simonw/llm-anthropic](https://github.com/simonw/llm-anthropic)（Apache License 2.0）的 `tests/cassettes/test_anthropic/test_stream_events_thinking.yaml` / `test_web_search.yaml` VCR cassette，经 gzip 解压 + 截断/脱敏后收录。原始 cassette 里没有 API key（pytest-recording 录制时已排除鉴权头），我们额外把 `web_search_tool_result` 里的 `encrypted_content` 不透明 blob 替换成占位字符串，并把结果数组截断到 1 条以控制文件体积。相关测试用例见 `internal/translator/openai_anthropic/translator_test.go` 里的 `TestStreaming_Thinking` / `TestTranslateResponse_Thinking` / `TestTranslateRequest_ThinkingRoundTrip`（用真实数据内联在测试里,不是从本文件读取）。

同一个 cassette 仓库的 `test_image_prompt.yaml` 提供了一张真实的 base64 PNG，用在 `openai_anthropic`/`anthropic_openai` 两个包的 `TestTranslateRequest_Image*` 系列测试里验证多模态图片透传（内联在测试代码中，未单独存成 fixture 文件）。
