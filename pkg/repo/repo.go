// Package repo 数据访问层：把"middleware 在请求路径上要查的数据"抽象成接口，
// 默认实现（file-backed / KV-backed）也放本包内。
//
// 边界——只收"按 key 查记录"型的接口：
//   - IdentityProvider     按凭证查 UserIdentity
//   - ModelServiceProvider 按 model 查 ModelServiceSnapshot
//   - EndpointProvider     按 model + group 选 Endpoint
//
// 不属于本包：
//   - Detector / Parser  纯解析逻辑（pkg/middleware）
//   - Moderator / BudgetGate 外部策略调用，非数据查询（pkg/middleware）
//
// middleware 通过 repo.XxxProvider 类型声明依赖；具体实现由 cmd 装配并注入。
package repo
