[English](README.md) | [简体中文](README.zh-CN.md)

# Documentation

The public documentation in this directory is maintained in English and Simplified Chinese.

## Index

| Area | English | 简体中文 |
|---|---|---|
| Product evolution | [Roadmap](ROADMAP.md) | [产品演进路线图](ROADMAP.zh-CN.md) |
| Architecture and contracts | [Architecture index](architecture/README.md) | [架构索引](architecture/README.zh-CN.md) |
| Architecture decisions | [ADR index](adr/README.md) | [架构决策记录索引](adr/README.zh-CN.md) |

## Bilingual maintenance convention

- Every public `*.md` document under `docs/` must have a sibling `*.zh-CN.md` document. The only exception is a generated artifact explicitly marked as generated.
- Both files start with an English / 简体中文 language switch.
- The two versions must preserve the same scope, heading hierarchy, code examples, identifiers, reason codes, and behavioral contracts.
- A change to public behavior is incomplete until both language versions are updated in the same pull request.
- Links in Chinese documents should prefer the Chinese target when one exists. Code symbols, paths, configuration keys, protocol names, and machine-readable values remain unchanged.
