[English](README.md) | [简体中文](README.zh-CN.md)

# 文档

本目录中的公开文档同时维护英文和简体中文版本。

## 索引

| 领域 | English | 简体中文 |
|---|---|---|
| 产品演进 | [Roadmap](ROADMAP.md) | [产品演进路线图](ROADMAP.zh-CN.md) |
| 架构与接口契约 | [Architecture index](architecture/README.md) | [架构索引](architecture/README.zh-CN.md) |
| 架构决策 | [ADR index](adr/README.md) | [架构决策记录索引](adr/README.zh-CN.md) |

## 双语维护约定

- `docs/` 下每一篇公开的 `*.md` 文档都必须有同目录的 `*.zh-CN.md` 中文版本；唯一例外是明确标注为自动生成的产物。
- 两个版本都必须在文件开头提供 English / 简体中文语言切换入口。
- 两个版本必须保持相同的内容范围、标题层级、代码示例、标识符、reason code 和行为契约。
- 公开行为发生变化时，必须在同一个 PR 中同步更新两个语言版本，否则该变更不算完成。
- 中文文档存在中文目标时应优先链接中文版本；代码符号、路径、配置键、协议名称和机器可读值保持不变。
