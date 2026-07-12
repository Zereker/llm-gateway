# 真实上游 VCR cassette 语料库

这里存的是第三方开源项目用 [pytest-recording](https://github.com/kiwicom/pytest-recording)（VCR.py）真实调用各厂商 API 录制下来的请求/响应 cassette 原文件，**原样收录**（未裁剪、未改写，仅去除了鉴权头——这是 pytest-recording 录制时自己做的，不是我们做的）。

## 为什么要存这个

验证 `internal/translator/*` 的协议转换是否正确，最有说服力的证据是真实厂商的请求/响应，而不是我们自己拍的假数据，也不是转述文档。这几个开源项目的测试套件里恰好有,而且用的都是允许再分发的宽松许可证（Apache 2.0 / MIT）。把它们存到仓库里,以后再核对某个字段的真实形状,直接翻这个目录,不用每次都重新上网找。

`testdata/fieldmatrix/upstream/` 那批文件是**精加工**的——针对某个具体 E2E 测试场景做过截断/脱敏/改名；这里的是**原始语料**，未经加工，覆盖面更广，供以后翻查、抽取、验证用。两者不是同一个东西，互不替代。

## 目录结构

```
vendor-cassettes/
├── anthropic/
│   ├── simonw-llm-anthropic/         # Apache License 2.0
│   └── langchain-ai-langchain/       # MIT License（langchain-anthropic 官方分包）
├── gemini/
│   └── simonw-llm-gemini/            # Apache License 2.0
├── cohere/
│   └── langchain-ai-langchain-cohere/ # MIT License
├── bedrock/
│   └── langchain-ai-langchain-aws/   # MIT License（langchain-aws 官方分包，Converse API）
└── openai/
    └── langchain-ai-langchain/       # MIT License（langchain-openai 官方分包）
```

每个来源目录下有一份该项目的 `LICENSE`（按 Apache 2.0 / MIT 的要求，再分发时必须保留许可证全文）。

## 来源与许可

### `anthropic/simonw-llm-anthropic/`（Apache License 2.0）

来自 [simonw/llm-anthropic](https://github.com/simonw/llm-anthropic) 的 `tests/cassettes/test_anthropic/`。

| 文件 | 内容 |
|---|---|
| `test_stream_events_thinking.yaml` | extended thinking 流式：`thinking_delta` → `signature_delta`（真实签名），usage 含 `cache_creation`/`service_tier`/`inference_geo` |
| `test_web_search.yaml` | server-side web search 工具：`server_tool_use` + `web_search_tool_result`（含 `encrypted_content`），usage 含 `server_tool_use.web_search_requests` |
| `test_image_prompt.yaml` | 多模态：真实的 base64 PNG image block |
| `test_tools.yaml` | 基础工具调用（非流式）：`tool_use` block、`input_schema` |
| `test_stream_events_tool_calls.yaml` | 基础工具调用（流式）：`input_json_delta` 增量参数 |

### `anthropic/langchain-ai-langchain/`（MIT License）

来自 [langchain-ai/langchain](https://github.com/langchain-ai/langchain) 官方 `langchain-anthropic` 分包 `libs/partners/anthropic/tests/cassettes/`（原文件是 gzip 压缩的 `.yaml.gz`，这里存的是解压后的 `.yaml`）。这批比 simonw 那批新且覆盖面更广，很多是我们尚未实现的 Claude 新特征：

| 文件 | 内容 | 我们的实现状态 |
|---|---|---|
| `test_citations.yaml` | citations：`content_block_location`（文档引用，无 URL）+ 流式 `citations_delta` | `web_search_result_location` 类型已支持（见下），本文件里的 `content_block_location` 类型无 URL，按设计丢弃 |
| `test_strict_tool_use.yaml` | 工具定义的 `strict` + `additionalProperties:false` | 已支持（`tools[].strict` 透传，两个方向） |
| `test_redacted_thinking.yaml` | `redacted_thinking` block（不透明、无签名，一次响应可能有多个且顺序必须保留） | **未实现**——当前 thinking 回传是单块两个 flat 字段的设计,处理不了 redacted_thinking 的多块顺序场景 |
| `test_code_execution.yaml` | `server_tool_use`（`bash_code_execution`）+ `bash_code_execution_tool_result`，响应顶层 `container` 字段 | **未实现**——这类 block 目前整体被静默丢弃 |
| `test_web_fetch.yaml` | `web_fetch` 工具 + `char_location`/`page_location` citations（引用抓取到的网页,无 URL 字段但有 document_index） | **未实现** |
| `test_search_result_tool_message.yaml` | `search_result` 类型的 tool_result block + `search_result_location` citations（无 URL） | **未实现**（citations 部分按设计丢弃,search_result block 本身未处理） |
| `test_remote_mcp.yaml` | `mcp_servers` 请求字段 + `mcp_tool_use`/`mcp_tool_result` block | **未实现**——请求侧目前传不进去（tools 循环只认 function 类型）,响应侧 block 被丢弃 |
| `test_tool_search.yaml` | `tool_search_tool_regex_20251119` 工具 + `tool_search_tool_result`，工具定义的 `defer_loading` | **未实现** |
| `test_programmatic_tool_use.yaml` | code execution 驱动 client tool（`caller`/`allowed_callers` 字段） | **未实现** |
| `test_streaming_tool_call_v1_v2_parity.yaml` | 基础流式 tool_use（`input_json_delta`），langchain 自己核对 SDK v1/v2 一致性用的 | 已支持（和我们现有实现一致,无新发现） |

### `gemini/simonw-llm-gemini/`（Apache License 2.0）

来自 [simonw/llm-gemini](https://github.com/simonw/llm-gemini) 的 `tests/cassettes/test_gemini/`。

| 文件 | 内容 |
|---|---|
| `test_tools_with_gemini_3_thought_signatures.yaml` | Gemini 3 的 `functionCall` part 上的 `thoughtSignature`（推理链签名,多轮历史必须原样带回） |
| `test_tools.yaml` | 基础 `functionDeclarations`/`functionCall`/`functionResponse` |
| `test_prompt_with_pydantic_schema.yaml` | `responseSchema`/`responseMimeType`（结构化输出） |

### `cohere/langchain-ai-langchain-cohere/`（MIT License）

来自 [langchain-ai/langchain-cohere](https://github.com/langchain-ai/langchain-cohere) 的 `libs/cohere/tests/integration_tests/cassettes/`。

| 文件 | 内容 |
|---|---|
| `test_invoke_tool_calls.yaml` | v2/chat 非流式工具调用：`tool_plan`/`tool_calls` |
| `test_streaming_tool_call.yaml` | v2/chat 流式工具调用：`tool-call-start`/`tool-call-delta`/`tool-call-end`/`tool-plan-delta` |
| `test_invoke_multiple_tools.yaml` | 并行多工具调用 |
| `test_invoke_with_vision_base64.yaml` | 多模态：`image_url` content part（base64） |
| `test_who_founded_cohere_with_custom_documents.yaml` | citations：`sources[].type=document`（无 URL，按设计丢弃） |
| `test_documents.yaml` | `documents` 请求字段（RAG，OpenAI 协议没有对应概念,不在我们翻译范围内） |
| `test_command_a_reasoning_with_tool_call.yaml` | `command-a-reasoning-08-2025` 的 `{"type":"thinking","thinking":...}` block（已支持，映射到 `reasoning_content`） |
| `test_stream.yaml` | 基础流式文本（`content-start`/`content-delta`/`content-end`），用来确认 content block 的 `type` 只在 `content-start` 出现一次 |

### `bedrock/langchain-ai-langchain-aws/`（MIT License）

来自 [langchain-ai/langchain-aws](https://github.com/langchain-ai/langchain-aws) 的 `libs/aws/tests/cassettes/`（原文件是 gzip 压缩的 `.yaml.gz`，**这里直接原样存的就是 `.yaml.gz`**——和上面 `anthropic/langchain-ai-langchain/` 先解压再存的做法不同，因为 `internal/cassette.Load` 现在支持整文件级别的透明 gunzip,不需要再手动解压一遍才能"原样收录"）。

这批录的是 Bedrock **Converse API**（`/model/{id}/converse`、`/converse-stream`），和 `internal/protocol/bedrock` 原有的 **InvokeModel** 路径（`protocol: anthropic`，body 直接是 Anthropic Messages 格式）是完全不同的 wire shape——对应 `internal/translator/openai_bedrock`（新增的 Converse 翻译器，`endpoint.protocol: bedrock`），不是 `openai_anthropic`。

| 文件 | 内容 | 我们的实现状态 |
|---|---|---|
| `test_agent_loop[v0/v1].yaml.gz` | 非流式工具调用（agent loop：调用工具→拿到结果→给出最终答案） | 已支持 |
| `test_agent_loop_streaming[v0/v1].yaml.gz` | 流式工具调用：`contentBlockStart`(`toolUse`)/`contentBlockDelta`(`toolUse.input` 增量 JSON 字符串)/`messageStop`/`metadata`(usage) | 已支持 |
| `test_thinking[v0/v1].yaml.gz` | extended thinking：`reasoningContent.reasoningText`（含签名，流式 `delta.reasoningContent.text`） | 响应侧文本已映射到 `reasoning_content`；签名本身及请求侧回传未处理（同 Anthropic 直连的 thinking 多轮场景），标记为 not-applicable |
| `test_citations[document0/1].yaml.gz` | citations：`document` 请求块 + 响应 `citationsContent`（`documentChar` 位置引用，无 URL） | **未实现**——同 Cohere citations 的既有先例，无 OpenAI 对应概念，标记为 not-applicable |
| `test_citations_v1[v0/v1].yaml.gz` | 同上，citations API v1 变体 | **未实现**，同上 |
| `test_pdf_citations.yaml.gz` | citations，引用来源是 PDF 文档 | **未实现**，同上 |

### `openai/langchain-ai-langchain/`（MIT License，99 个文件）

来自 [langchain-ai/langchain](https://github.com/langchain-ai/langchain) 官方 `langchain-openai` 分包 `libs/partners/openai/tests/cassettes/`。文件数量大,分类索引和"大多数文件实际走 Responses API 而不是 Chat Completions"这个反直觉发现单独写在 `openai/langchain-ai-langchain/README.md` 里,这里不重复。一句话概括：真正的 Chat Completions（`"messages"` 形状）样本只有 2 个；剩下 97 个全是 Responses API 形状,覆盖工具调用、内置服务端工具（MCP/code interpreter/web search/file search/apply patch/tool search）、reasoning、结构化输出、多轮压缩、图像生成等，是目前语料库里覆盖面最广的一批。

## 怎么用

字段形状有疑问时，直接用 `python3 -c` 读（大部分是纯文本 YAML；个别响应体是 gzip 压缩的 bytes，需要 `gzip.decompress`）：

```python
import yaml, gzip
with open("anthropic/langchain-ai-langchain/test_citations.yaml") as f:
    data = yaml.safe_load(f)
# 新格式（requests/responses 两个平行数组，langchain-ai/langchain 官方套件用这个）：
req, resp = data["requests"][0], data["responses"][0]
body = req["body"]  # bytes，可能需要 gzip.decompress
# 旧格式（interactions 数组，simonw 系列插件 + langchain-cohere 用这个）：
# data["interactions"][0]["request"] / ["response"]
```

**不要**把这里的文件当成我们自己的 API 测试固件去跑（它们不是为我们的 schema 设计的）；它们是**协议形状的参照**——某个字段该长什么样,拿这里的真实数据核对,而不是凭印象猜。

## 自己录制真实数据（`scripts/record-cassette`）

有些厂商（DeepSeek / 智谱 GLM / MiniMax——两轮系统性搜索确认）在开源社区里**不存在**任何可收录的真实录制数据：各生态要么 mock、要么直接打真实 API（key-gated 不留档）、要么（litellm）录进带 TTL 的 Redis 缓存而非提交文件。对这类厂商，用仓库里的录制工具自己打一次真实 API 录下来：

```sh
echo '{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}' > /tmp/req.json
RECORD_API_KEY=sk-... go run ./scripts/record-cassette \
  -url https://api.deepseek.com/chat/completions \
  -body-file /tmp/req.json \
  -vendor deepseek -model deepseek-chat -name chat_basic
# -> testdata/vendor-cassettes/deepseek/deepseek-chat/openai/nostream/chat_basic.yaml
```

- **目录布局由工具自动生成**,自录数据的标准层级是 `<vendor>/<model>/<protocol>/<stream|nostream>/<场景名>.yaml`：vendor/model/场景名来自 flag,protocol 默认 `openai`（`-protocol` 覆盖）,流式桶直接读请求体自己的 `"stream"` 字段,不用重复声明。第三方来源目录（`anthropic/simonw-llm-anthropic/` 这批）**不**套这个层级——它们是按来源仓库原样收录的（单个文件内部常混流式/非流式多次交互,物理上放不进单一桶）,溯源和 LICENSE 都挂在来源目录上,两套布局并存、按"自录 vs 收录"区分。
- key 只从环境变量读（默认 `RECORD_API_KEY`），不进 shell history；落盘前按 header 名（`authorization`/`x-api-key`/…）**和** key 字面值双重脱敏成 `**REDACTED**`（实现见 `internal/cassette/recorder`，其单测证明写出的文件能被 `cassette.Load` 原样读回）。
- 认证方式：`-auth bearer`（默认）/ `x-api-key` / `api-key` / `query:<param>`（key 在 URL 上，如 Gemini AI Studio 的 `query:key`）/ `none`；协议要求的额外头用 `-header "anthropic-version: 2023-06-01"`（可重复）。
- 多轮对话（工具调用回环）：第二次调用加 `-append`，追加到同一个 cassette；多轮场景归属它**第一轮**落进的桶（第二轮常是非流式,文件不会因此搬家——桶分类的是场景,不是单次交互）。
- 上游返回非 2xx 时**不落盘**（错误响应也是真实数据,但要求操作者看过错误、修好请求重录,而不是把报错默默提交进语料库）。
- 自录数据没有第三方 LICENSE,在本 README 里记一行"何时、对哪个模型、录了什么"即可。
- **提交前必须人工通读文件再 grep 一遍**——工具脱敏的是它认识的凭证,响应体里如果回显了别的敏感信息,工具不知道。

## 注意

- 这些 cassette 是第三方项目公开发布、允许再分发的测试固件（Apache 2.0 / MIT），不是我们调用真实 API 录制的（`self-recorded/` 目录除外,见上节）——`testdata/fieldmatrix/upstream/` 里经过脱敏/裁剪的衍生 fixture 才是我们主动整理过的。
- pytest-recording 录制时已经把鉴权头（`x-api-key`/`authorization`）替换成 `**REDACTED**`，我们额外核对过一遍全部文件，没有发现任何真实密钥、token 或签名 URL。
- 后续如果又找到新的真实数据源，照这个模式加：新开一个 `<vendor>/<source-repo>/` 目录，把原始 cassette 原样存进去,附上 LICENSE，在这份 README 里补一行说明覆盖了什么、以及我们的实现现状。
