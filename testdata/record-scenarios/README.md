# record-scenarios/ — 录制用的标准请求场景包

`scripts/record-cassette -scenario-dir` 批量模式的输入：对一个新厂商录制时,不再手写最小化请求,而是把这里的**每个**场景依次打到该厂商的真实 API 上录下来——这样自录 cassette 的请求侧覆盖面对齐真实 SDK 的参数面,而不是取决于当时随手写了什么。这正是 langchain / simonw 那批第三方 cassette 有价值的原因(请求体是真实 SDK 生成的),也是自录数据最容易悄悄丢掉的东西。

录制时 `"model"` 字段由 `-model` flag 替换,其余字节原样发送。

## `openai-chat/` 场景与来源

每个场景的请求体都**不是拍脑袋写的**,来源分三类:真实 SDK 请求原文(verbatim)、真实 SDK 请求的最小改动(derived)、以及本仓库维护的全参数矩阵(curated)：

| 场景 | 覆盖的参数面 | 来源 |
|---|---|---|
| `chat_basic.json` | 非流式最小对话 | derived：`vendor-cassettes/openai/langchain-ai-langchain/TestOpenAIStandard.test_stream_time.yaml` #0 的真实 ChatOpenAI 请求,去掉 `stream`/`stream_options` |
| `chat_stream_usage.json` | `stream` + `stream_options.include_usage` | verbatim：同上文件 #0,一字未改 |
| `tools_named_choice_stream.json` | `tools` + **指名** `tool_choice` + `temperature:0` + 流式 | verbatim：`test_streaming_tool_call_v1_v2_parity.yaml` #0 |
| `structured_output_json_schema.json` | `response_format: json_schema`（`strict:true` + `additionalProperties:false`）+ 显式 `stream:false` | verbatim：`test_schema_parsing_failures.yaml` #2 |
| `tool_loop_round_trip.json` | 多轮工具回环：assistant 带**并行双** `tool_calls` + 两条 `role:"tool"` 结果——翻译层最容易出 bug 的形状 | derived：`vendor-cassettes/anthropic/simonw-llm-anthropic/test_tools.yaml` #1 的真实 Anthropic SDK 请求,经本仓库 `anthropic_openai.TranslateRequest` 转成 OpenAI 形状（数据源头仍是真实 SDK） |
| `chat_full_params.json` | 全参数矩阵（23 个顶层字段：`n`/`seed`/`stop`/`presence_penalty`/`modalities`/`prediction`/`web_search_options`/…） | curated：`fieldmatrix/chat-full.json` 的副本（该文件本身就是"真实上游已知接受的每个字段"的矩阵）。部分严格校验的厂商可能对未知字段返回 4xx——批量模式会跳过该场景继续,报错留给操作者判断 |

**覆盖面由测试强制**：`internal/cassette/scenario` 的单测要求本目录合集覆盖 `fieldmatrix/chat-full.json` 的每一个顶层字段,并且流式/非流式、工具定义、工具结果回环、`response_format` 各至少有一个场景承载——删场景删掉了唯一承载某字段的文件,测试直接红。

## 对新厂商批量录制

```sh
RECORD_API_KEY=sk-... go run ./scripts/record-cassette \
  -url https://api.deepseek.com/chat/completions \
  -scenario-dir testdata/record-scenarios/openai-chat \
  -vendor deepseek -model deepseek-chat
# -> testdata/vendor-cassettes/deepseek/deepseek-chat/openai/{stream,nostream}/<场景名>.yaml
```

某个场景被上游拒绝(非 2xx)时会跳过并继续,最后汇总报告;录完照旧:通读 + grep 脱敏检查,再在 `vendor-cassettes/README.md` 登记。

## 加新场景

来源优先级不变：真实 SDK 请求原文 > 真实数据经本仓库 translator 派生 > 手写。手写的必须在上表标注清楚,并说明为什么找不到真实来源。
