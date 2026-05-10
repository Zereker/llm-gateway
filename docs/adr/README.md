# Architecture Decision Records (ADR)

本目录记录 llm-gateway **架构层面**的决策。bug fix / 实现细节走 commit message + PR 描述；架构选择、组件边界、向后不兼容改动走 ADR。

> ADR 是**决策的过程记录**，不是 spec。每条 ADR 描述：当时的现状、考虑过的选项、为什么选了某个、放弃的代价。后人接手时知道"为什么不这么做"，避免回头路。

## 与 `docs/architecture/` 的分工

| 目录 | 性质 | 何时写 |
|---|---|---|
| `docs/architecture/00-07*.md` | 当前**有效**的设计规范（接口契约、组件职责） | 实现就要符合；改了文档同步改代码 |
| `docs/adr/####-*.md` | **历史决策**的记录（含被拒方案 + 取舍）| 提议变更时写；被采纳后归档 |

`architecture/` 是"现在长什么样"，`adr/` 是"为什么长这样、曾考虑过怎样的样"。

## 状态流转

```
  Proposed ──→ Accepted ──→ Deprecated / Superseded by NNNN
      │
      └──→ Rejected
```

- **Proposed**：作者提交，等待 review；可改可撤。
- **Accepted**：团队通过，开始实施；ADR 内容定稿，不再改（除元数据）。后续若需修改决策，写**新** ADR 标注 supersedes。
- **Rejected**：讨论后未通过，保留作为"曾考虑"的证据。
- **Deprecated**：决策本身被废弃但没替代品（如组件被删除）。
- **Superseded by NNNN**：被新 ADR 替代；保留旧 ADR 作为历史。

**重要**：Accepted 之后不要直接编辑内容（除了上面五个 status 行）；要改只能写新 ADR supersede。这是 ADR 跟普通文档的本质区别——历史决策不能被改写。

## 文件命名

```
####-kebab-case-title.md
```

`####` 是 4 位流水号（0001 起），全局唯一不复用。即便 ADR 被 Reject 也保留编号。

## 模板（写新 ADR 时拷贝）

```markdown
# NNNN. <短标题>

* **Status**: Proposed
* **Date**: YYYY-MM-DD
* **Author**: <github handle>

## Context

为什么需要这个决策？现状是什么？哪些约束 / 痛点驱动它？

引用具体代码位置：`pkg/foo/bar.go:42`、commit hash、issue 编号。

## Options Considered

### Option A: <短描述>
- **怎么做**：...
- **正面**：...
- **负面**：...

### Option B: <短描述>
（同上）

## Decision

我们选 **Option X**。

理由：
- ...

## Consequences

### Positive
- ...

### Negative / Trade-offs
- ...

### Migration Path（若涉及向后不兼容）

阶段 1：...
阶段 2：...
回退方案：...
```

## 当前 ADR 索引

| # | 标题 | 状态 | 摘要 |
|---|---|---|---|
| [0001](0001-domain-repo-layering.md) | `pkg/domain` 与 `pkg/repo` 分层 | Proposed | 修复 domain 反向 import repo 的分层颠倒 |
| [0002](0002-canonical-schema.md) | Canonical request schema 决策 | Proposed | Canonical 已被删，需明确"复活"还是"承认 N×M 成本" |
| [0003](0003-schedule-ctx-as-parameter.md) | `Selection` 不持有 ctx | Proposed | 把 ctx 从 Selection 字段改为 Pick/Report 参数 |
| [0004](0004-schedule-load-fallback.md) | `LoadFallback` 装配位置 | Proposed | 从 per-Request 字段移到 Scheduler-level Config |
