[English](README.md) | [简体中文](README.zh-CN.md)

# 架构决策记录 (ADR)

该目录记录 llm-gateway 的**架构级**决策。 Bug 修复/实施细节通过提交消息 + PR 描述；架构选择、组件边界和向后不兼容的更改都会通过 ADR。

> ADR 是**决策过程的记录**，而不是规范。每份 ADR 都描述了：当时的情况、考虑的选项、选择某项选项的原因以及放弃选项的成本。这让继任者知道“为什么不这样做”，避免重蹈覆辙。

## 与 `docs/architecture/` 的分工

|目录 |自然 |什么时候写|
|---|---|---|
| `docs/architecture/*.md` |当前**有效的**设计规范（接口契约、组件职责）|实施必须符合它；当文档更改时，同步更新代码 |
| `docs/adr/####-*.md` | **历史决策**的记录（包括被拒绝的选项+权衡）|提出变更时所写；采用后存档 |

`architecture/` 是“现在的样子”； `adr/` 是“为什么它看起来是这样的，以及曾经考虑过哪些替代方案。”

## 状态生命周期

```
  Proposed ──→ Accepted ──→ Deprecated / Superseded by NNNN
      │
      └──→ Rejected
```

- **提议**：由作者提交，等待审核；仍然可以更改或撤销。
- **已接受**：团队批准，开始实施； ADR 内容是最终的，不再更改（元数据除外）。如果稍后需要更改决定，请编写一个标记为取代的**新** ADR。
- **拒绝**：讨论过但未批准；作为“曾经考虑过”的证据。
- **已弃用**：该决定本身已被放弃，没有替代（例如，该组件已被删除）。
- **被 NNNN 取代**：被新的 ADR 取代；旧的 ADR 将作为历史保留。

**重要**：Accepted后请勿直接编辑内容（以上五行状态行除外）；要更改它，您只能编写一个新的 ADR 来取代它。这是ADR与普通文件的本质区别——历史决策不能被重写。

## 文件命名

```
####-kebab-case-title.md
```

`####` 是一个 4 位序列号（从 0001 开始），全局唯一且永不重复使用。即使 ADR 被拒绝，其编号仍会保留。

## 模板（编写新 ADR 时复制此模板）

```markdown
# NNNN. <Short title>

* **Status**: Proposed
* **Date**: YYYY-MM-DD
* **Author**: <github handle>

## Context

Why is this decision needed? What is the current situation? What constraints / pain points are driving it?

Reference specific code locations: `internal/foo/bar.go:42`, commit hash, issue number.

## Options Considered

### Option A: <Short description>
- **How**: ...
- **Pros**: ...
- **Cons**: ...

### Option B: <Short description>
(same as above)

## Decision

We choose **Option X**.

Rationale:
- ...

## Consequences

### Positive
- ...

### Negative / Trade-offs
- ...

### Migration Path (if backward-incompatible)

Phase 1: ...
Phase 2: ...
Rollback plan: ...
```

## 当前 ADR 指数

|美国存托凭证 |状态 |决定|
|---|---|---|
| [0001](0001-explainable-virtual-model-routing.zh-CN.md) |已接受 |调度前解决虚拟模型策略 |
