# configs/

每个子目录是一份**完整的、自包含的**网关配置，对应一个环境。

## 结构

```
configs/
├── local/                  # 零依赖本地开发；可直接 git clone 后跑
│   ├── gateway.yaml        # server / middleware / paths / database
│   ├── apikeys.json        # map[apiKey]UserIdentity（仍 file-based）
│   └── gateway.db          # sqlite 数据库（首次 boot 自动创建；空表）
│
└── prod/                   # 生产模板
    └── gateway.yaml        # paths 指向 /etc/ai-gateway/...；database 推 postgres
```

`gateway.db` 是 sqlite 文件，首次启动 gateway 时由 `infra.Migrate` 自动创建并建表
（model_services / endpoints）。表里的数据由 admin（`cmd/admin`）维护。

## 路径解析

`gateway.yaml` 的 `paths.apikeys` 和 `database.dsn`（仅 sqlite 文件路径）是
**相对 yaml 文件位置**解析的（详见 `pkg/config.Load`）。
`local/gateway.yaml` 写 `dsn: gateway.db` 就指向 `configs/local/gateway.db`，跟 CWD 无关。

→ 整个 env 目录可整体复制到任何机器（如 `/etc/ai-gateway/`），结构不变就能用。

`":memory:"`、绝对路径、`postgres://...` URL 都不会被相对解析。

## 添加新环境

```sh
cp -r configs/local configs/staging
# 编辑 configs/staging/gateway.yaml + apikeys.json
# 用 cmd/admin 给 staging DB 录入 model_service / endpoint
go run ./cmd/gateway -config ./configs/staging/gateway.yaml
```

## 密钥管理

**绝对不要把真实密钥 commit 到 git。** 推荐：

| 场景 | 方案 |
|------|------|
| local dev | `apikeys.json` 用测试 key（如 `sk-test-xxx`），committable；DB 由 admin 录入测试 endpoint |
| CI / staging | 部署脚本从 CI secret 渲染 apikeys.json；DB 用独立 staging 实例 |
| prod | k8s Secret 挂载 apikeys.json 到 `/etc/ai-gateway/`；DB 用独立 postgres，凭证走 Vault / secret manager |

`prod/gateway.yaml` 默认 `database.driver: postgres` 并占位一个 DSN，部署时替换。
endpoint 的 `api_key` 字段以明文存 DB（v0.1）；后续可加 KMS / sealed-secret 包裹。
