# 0002. Canonical request schema 的存废

* **Status**: Proposed
* **Date**: 2026-05-05
* **Author**: zhanghaojie.114

## Context

`docs/architecture/00-overview.md` 第 3 章 P4 原则明确写：

> **Canonical request schema** — 内部用一套规范化请求结构（`CanonicalRequest`，OpenAI 兼容形态）作为通用语言；客户端协议 → Canonical 由 `Envelope` 完成；Canonical → 上游协议由 `Adapter.TransformRequest` 完成。

但代码事实跟原则相反：

```go
// pkg/domain/envelope.go:10
// 业务逻辑在 RawBytes（原始字节）+ Model（M3 从 body 提取的 model 字段）+
// SourceProtocol / Modality 上做决策；body 翻译 / 字段映射全部下放给 pkg/translator
// 各 translator 实现，本结构**不**承载 canonical 化职责。
//
// 设计精神：M3 只做"读 body + 拿 model 做路由"，不做参数解析；CanonicalRequest
// 这种"统一 internal 表示"曾经存在但全字段无消费者，已删（v1.0 review 决定）。
// 上游 / 客户端协议 shape 转换由 pkg/translator/<src>_<tgt>/ 各自处理。
```

也就是说 Canonical schema 在 v1.0 review 时被删了，但 `00-overview.md` 的设计原则没同步更新——**设计文档与代码漂移**。

### 实际后果：N×M 协议矩阵

每对 `(source, target)` 协议要一个 translator 子包：

```
pkg/translator/
├── identity/           anthropic.go / openai.go / responses.go  (3 对同协议)
├── anthropic_openai/   translator.go
├── openai_anthropic/   translator.go
└── openai_gemini/      translator.go
```

当前 5 个 translator 实现，覆盖 5 对协议。客户端 3 协议（OpenAI Chat / Anthropic Messages / OpenAI Responses）× 上游 4 协议（OpenAI / Anthropic / Gemini / Bedrock）= 12 对，目前缺 7 对（responses → anthropic / responses → gemini / 任意 → bedrock 等）。

接新上游（如 Bedrock / Vertex Native / Mistral）的成本是 **N 个新 translator 子包**（每个客户端协议各一个），而不是常数级 1 个。

### 已经局部"复活"的 Canonical 思路

usage 提取层已经按 Canonical 思路抽出来了：

```go
// pkg/usage/extractor/extractor.go:1
// 把"按协议从上游响应里提 usage"的逻辑从 translator 抽出来共享。
//
// 5 个 translator 的 ResponseHandler 里都散落着 usage 解析——但按上游协议
// 归一只有 3 套（OpenAI / Anthropic / Gemini），每套被 2 个 translator 重复实现。
// 抽出来后 translator 只关心"翻译 chunk → client format"，usage 提取走 side-channel。
```

即 usage 提取层证明了"按协议归一"是有价值的——只是这件事在 request/response shape 维度没做。

## Options Considered

### Option A: 复活 Canonical schema

一个内部统一形态（OpenAI ChatCompletion 兼容），所有 translator 拆成两段：
- `ClientToCanonical`：N 个（每客户端协议一个）
- `CanonicalToUpstream`：M 个（每上游协议一个）

矩阵从 N×M 降到 N+M。

接新上游 = 1 个 `CanonicalToUpstream` 实现；接新客户端协议 = 1 个 `ClientToCanonical` 实现。

- **正面**：
  - 接入新厂商的边际成本从 N 降到 1。
  - 可在 Canonical 层统一做 prompt rewrite / context truncation / tool schema normalization。
  - 跟 `usage/extractor` 已有的"按协议归一"思路对齐。
  - 重新对齐 P4 原则与代码事实。
- **负面**：
  - **抽象泄漏风险高**：Canonical 必须能表达所有源 / 目标协议的全集（function calling / vision / multi-turn tool use / Anthropic 独有 system block 等），任何一边引入新能力都要扩 Canonical schema，否则丢信息。
  - 双跳翻译 = 双倍 bug 面：A → Canonical → B 和 A → B 直翻在 edge case 表现可能不同。
  - 流式重组复杂：每个 chunk 要 deserialize → Canonical event → 再 serialize 到上游格式，性能 + 状态机维护成本上升。
  - 工作量预估：5-10 人周（含全部现有 translator 重写 + 流式翻译）。

### Option B: 承认 N×M 成本，接受现状

明确"每对协议一个 translator 子包"是既定方针，更新 `00-overview.md` 删除 P4 关于 Canonical 的描述，提供工具降低单 translator 的写作成本。

- **正面**：
  - 直翻 (A → B) 控制力最强，每对独立实现可以做协议特定优化（Anthropic 的 system block 直接映到 OpenAI system message，不绕弯）。
  - 流式翻译可以做单跳 chunk-level 翻译（不需要重构成 event stream 中间态）。
  - 文档跟代码对齐，没有"理想 vs 现实"撕裂。
  - 工作量小：补文档 + 写 scaffold 工具。
- **负面**：
  - 接新上游成本是 N（不是 1），增长非线性。
  - 跨协议调试时要看多个独立实现，无统一 reference。
  - Translator 子包数量随时间膨胀（5 → 12 → 更多）。

### Option C: 混合——按"客户端协议族"建 Canonical，但只覆盖头部场景

定义一个"**OpenAI Chat 兼容 Canonical**"（不试图涵盖 vision / 复杂 tool use），其它协议（Anthropic Messages / Responses / 跨协议高级特性）继续走直翻 translator。

- **正面**：
  - 头部用例（OpenAI client → 任意上游 chat）走 Canonical 路径，N+M 受益。
  - 长尾 / 特殊能力走直翻，避免 Canonical 抽象过载。
- **负面**：
  - 用户得知道自己请求走的是哪条路径（Canonical or 直翻），否则报障难复现。
  - 维护两套并行机制，复杂度比单选更高。
  - "什么时候走 Canonical / 什么时候直翻" 的分流规则会随时间漂移，最终回到混乱状态。

## Decision

**未决（待团队讨论）**——本 ADR 的目的是把选项摆在桌面上，由 architect / tech lead 在 review meeting 决策。

### 决策建议清单

讨论时建议覆盖这几个问题：

1. **未来 6 个月内要接的上游数量**？如果 ≥ 3 个新上游 → Option A 收益大。
2. **每个新上游的协议是否相互正交**？如果 Bedrock / Vertex / Mistral 协议高度相似（都是 OpenAI-like），N×M 的 N 实际不大 → Option B 够用。
3. **流式翻译能不能接受双跳成本**？P99 延迟敏感场景下，Canonical 中间态序列化可能加 1-5ms。
4. **现有团队对"协议抽象层"维护意愿**？Canonical schema 要专人 own（schema 演进、向后兼容）；没人 own 就别选 A。
5. **是否打算长期暴露 Anthropic Messages / Gemini native 协议**？如果只为兼容 SDK 而存在，长尾部分用直翻就够了。

### 推荐的暂时选择

如果团队**无法立即决策**，临时位置（直到下一个新上游接入前）：
- 接受 Option B 现状不动代码。
- **必须改文档**：`00-overview.md` 第 3 章 P4 改成"每对 (源, 目标) 协议独立 translator 子包"，承认现实，避免文档误导。
- 接下一个新上游时被迫重新评估（届时增量成本是否可接受）。

## Consequences

### Option A 被采纳

- Positive：长期维护成本曲线变平；新厂商接入快；Canonical 成为 prompt 工程 / 上下文管理的统一切入点。
- Negative：6+ 周专项工作量；流式翻译复杂度；Canonical schema 治理成开放问题。
- Migration Path：
  1. 阶段 1（2 周）：定义 Canonical schema（基于 OpenAI Chat 形态扩展，明确支持矩阵）。
  2. 阶段 2（2 周）：写一个 `ClientToCanonical` + 一个 `CanonicalToUpstream` 端到端验证。
  3. 阶段 3（4 周）：现有 5 个 translator 拆成 N+M 形态，feature parity 测试。
  4. 阶段 4：删除直翻 translator 子包，统一走 Canonical。
  5. 回退：阶段 3 之前每阶段独立 commit；阶段 4 一次性切换可由 feature flag 控制并行运行。

### Option B 被采纳

- Positive：0 代码工作量；流式 / 边界 case 控制力最强；每个 translator 是独立单元好测试。
- Negative：文档要修正（删除 Canonical 的 P4 描述）；接每个新厂商成本固定为 N。
- Migration Path：
  1. 改 `docs/architecture/00-overview.md` 第 3 章 P4 描述。
  2. 改 `docs/architecture/02-protocol-translation.md`，明确 N×M 模式 + 每个 translator 子包结构 + 流式翻译规范。
  3. 写 `cmd/scaffold-translator` codegen 工具：从 source / target 协议元信息生成 translator 骨架（Source / Target / TranslateRequest / NewResponseHandler 四个方法 + 测试用例模板）。
  4. 提供 vendor onboarding 文档（与 ADR 对应的 docs/contributing/onboard-vendor.md）。

### Option C 被采纳

- 不推荐，但若选：必须先写决策文档明确"Canonical 路径覆盖范围 = ?"，否则两套机制会互相蚕食。
