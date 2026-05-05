# 0001. `pkg/domain` 与 `pkg/repo` 分层

* **Status**: Proposed
* **Date**: 2026-05-05
* **Author**: zhanghaojie.114

## Context

按 `docs/architecture/00-overview.md` 第 3 章设计原则、`01-request-pipeline.md` 第 1 节包结构所述，`pkg/domain` 应是网关的**纯领域类型层**——不依赖任何持久化 / 框架细节，是上层所有包共同的语言。

但代码事实跟设计相反：

```go
// pkg/domain/endpoint.go
package domain
import "github.com/zereker-labs/ai-gateway/pkg/repo"
type (
    Endpoint             = repo.Endpoint
    EndpointCapabilities = repo.EndpointCapabilities
    EndpointForm         = repo.EndpointForm
)

// pkg/domain/identity.go
type (
    UserIdentity = repo.UserIdentity
    Credentials  = repo.Credentials
)

// pkg/domain/model_service.go
type ModelService = repo.ModelService

// pkg/domain/secret.go
type Secret = repo.Secret
```

即 `pkg/domain` **直接 import** `pkg/repo`，并把核心实体类型用 type alias 暴露。`go list -deps ./pkg/domain` 输出包含 `pkg/repo`。

`pkg/repo/models.go:130` 才是 `Endpoint` 的真定义——带 sqlx `db:"..."` tag、gorm `gorm:"..."` 双标签，以及 `Scanner`/`Valuer` 实现。`pkg/domain/endpoint.go:5` 注释直白承认："真实定义在 pkg/repo（带 sqlx + gorm tag、Scanner/Valuer），这里保留 alias 让 middleware / adapter / schedule / ratelimit 等现有代码不需要改 import。"

### 实际后果

`go list -deps` 显示：
- `pkg/schedule` 依赖闭包包含 `pkg/repo`（应为纯算法层、不该知道 SQL）
- `pkg/translator` 依赖 `pkg/repo`（应为协议翻译、不该知道存储）
- `pkg/upstream` 依赖 `pkg/repo`（HTTP / 流式层，同上）

任何想换持久化（Dynamo / etcd / 内存）必须修改"领域类型本身"，违反 P5 "Pluggable infrastructure" 原则。

### 历史背景

从注释看，alias 模式是在某次重构中**为了避免大量 import 路径修改**而引入的过渡方案。但既然 `domain` 包已经存在并被广泛 import，这层 alias 给人"已经有 domain 层"的错觉，掩盖了真实的依赖方向，使后续维护者判断错误。

## Options Considered

### Option A: 把真定义搬回 `pkg/domain`，`pkg/repo` 反向 import

`pkg/domain/endpoint.go` 持有纯净 struct（无 db tag）；`pkg/repo` 引入 row 类型或嵌入 struct 承载 ORM tag + Scanner/Valuer。

```
pkg/domain/endpoint.go      → struct 真定义
pkg/repo/endpoint_row.go    → 内嵌 / 转换层 + sqlx tag + Scanner/Valuer
```

- **正面**：分层方向恢复正确（infra ← repo ← domain ← middleware）；纯领域类型可独立测试 / 序列化。
- **负面**：repo 包要做 row → domain 转换；现有 ORM tag 直接挂在 domain 类型上的简洁性丢失；EndpointCapabilities / EndpointForm / AuthConfig 等带 Scanner/Valuer 的复杂字段也需要拆分定义。预估工作量 1-2 人天。

### Option B: 承认 `pkg/repo` 是 domain 真实家、删除 `pkg/domain` 的 alias

让上层包直接 `import pkg/repo` 引用 `repo.Endpoint` 等。

- **正面**：分层诚实，0 行新代码；删除 alias 文件即可。
- **负面**：包名 "repo" 不优雅——schedule / translator 这些"业务层"包 import "repo" 在概念上别扭；视觉上让代码看起来"什么都依赖存储"。

### Option C: 保持现状

什么都不做，把"domain alias repo"当成隐式约定。

- **正面**：0 工作量。
- **负面**：分层错误持续；新人首次读代码会被误导；将来任何持久化层重构都被这层 alias 拖累。

### Option D: 拆出 `pkg/domain` 真定义 + 在 `pkg/repo` 用 builder/mapper

`pkg/domain` 持纯类型；`pkg/repo` 不嵌入而是定义 `EndpointRow` 独立 struct + `EndpointRow.ToDomain()` / `Endpoint.ToRow()` 方法。

- **正面**：完全解耦；测试时可不经 repo 层。
- **负面**：每个实体多写 50-100 行 mapper；复杂字段（typed JSON 列）的转换重复劳动。工作量 3-5 人天。

## Decision

**采纳 Option A**（把真定义搬回 `pkg/domain`，`pkg/repo` 通过类型嵌入承载 ORM tag）。

理由：
- 分层诚实是底线（Option C 排除）。
- Option B 让"repo"包名出现在业务层 import 列表，违反"领域语言"原则；包名上的别扭是真实的认知负担，不是矫情。
- Option D 增加 mapper 样板，对当前规模（~6 个核心实体）收益不抵成本。
- Option A 折中：domain 持纯类型，repo 用结构嵌入加 ORM tag，Scanner/Valuer 留在 repo 包，迁移工作量可控。

## Consequences

### Positive

- `pkg/schedule` / `pkg/translator` / `pkg/upstream` 的依赖闭包不再包含 `pkg/repo`——纯算法 / 翻译 / IO 层不知道 SQL。
- 任何想换持久化方案（DynamoDB / etcd / 测试用 in-memory）都不动 domain 类型，只需新写一套 repo。
- 新人读 `go list -deps ./pkg/middleware` 时分层一目了然。

### Negative / Trade-offs

- repo 包多 50-150 行嵌入 / Scanner-Valuer 代码（每个实体）。
- 既有所有引用 `domain.Endpoint`、`domain.UserIdentity` 的地方继续工作（API 不变），但 repo 内部代码要调整。
- `EndpointCapabilities` 等 typed JSON 列的 Scanner/Valuer 拆分需要细致 review。

### Migration Path

**阶段 1：批量动 1 个实体（验证形态）**
1. 选 `Endpoint`（最复杂、最能暴露问题）作为试点。
2. 在 `pkg/domain/endpoint.go` 定义纯 struct（无 tag、无 Scanner/Valuer）。
3. 在 `pkg/repo/endpoint_row.go` 定义 `endpointRow` 内嵌 `domain.Endpoint` + 加 `db:` / `gorm:` tag + Scanner/Valuer 实现。
4. `SQLEndpointReader` 改用 `endpointRow` 做 sqlx select、转 `*domain.Endpoint` 返回。
5. 删除 `pkg/domain/endpoint.go` 中的 `type Endpoint = repo.Endpoint` alias。
6. 跑 `go test ./...` 验证全绿。

**阶段 2：复制阶段 1 形态搬其他实体**
- 顺序：`UserIdentity` / `Credentials` / `ModelService` / `Secret`、`EndpointCapabilities` / `EndpointForm`。
- 每个实体一个独立 commit，便于 bisect 和 rollback。

**阶段 3：清理 + 验证**
- `grep -r "= repo\." pkg/domain/` 应该返回空。
- `go list -deps ./pkg/schedule | grep repo` 应该返回空。
- `go list -deps ./pkg/translator | grep repo` 应该返回空。
- 跑 `make test-integration`。

**回退**：每个阶段独立 commit；任意阶段发现回归直接 `git revert`，先前阶段保留。
