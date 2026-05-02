# configs/

每个子目录是一份**完整的、自包含的**网关配置，对应一个环境。

## 结构

```
configs/
├── local/                   # 零依赖本地开发；可直接 git clone 后跑
│   ├── gateway.yaml         # server / middleware / paths
│   ├── apikeys.json         # map[apiKey]UserIdentity
│   └── kv/                  # store.FileKV 根目录
│       ├── modelservice/    # 一个文件一个 ModelServiceSnapshot
│       └── endpoint/        # 一个文件一个 Endpoint（含真实 upstream API key）
│
└── prod/                    # 生产模板
    └── gateway.yaml         # paths 指向 /etc/ai-gateway/...；真实数据由部署侧挂载
```

## 路径解析

`gateway.yaml` 里的 `paths.apikeys` / `paths.kv_root` 是**相对 yaml 文件位置**
解析的（详见 `pkg/config.Load`）。`local/gateway.yaml` 写 `apikeys: apikeys.json`
就指向 `configs/local/apikeys.json`，跟 CWD 无关。

→ 整个 env 目录可整体复制到任何机器（如 `/etc/ai-gateway/`），结构不变就能用。

## 添加新环境

```sh
cp -r configs/local configs/staging
# 编辑 configs/staging/gateway.yaml + apikeys.json + kv/endpoint/*.json
go run ./cmd/gateway -config ./configs/staging/gateway.yaml
```

## 密钥管理

**绝对不要把真实密钥 commit 到 git。** 推荐：

| 场景 | 方案 |
|------|------|
| local dev | `apikeys.json` + `kv/endpoint/*.json` 用测试 key（如 `sk-test-xxx`），committable |
| CI / staging | 部署脚本从 CI secret 渲染 |
| prod | k8s Secret 挂载到 `/etc/ai-gateway/`；或 Vault sidecar；或 deploy-time 渲染 |

`prod/gateway.yaml` 的 `paths` 指向标准生产路径（`/etc/ai-gateway/`），yaml 本身可
committable，但 yaml 引用的 `apikeys.json` / `kv/endpoint/*.json` 必须由部署侧提供。
