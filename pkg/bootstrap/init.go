// Package bootstrap 集中处理启动装配 + 自检 + 优雅关闭。
//
// cmd/gateway/main.go 调用 Init / Shutdown；TODO: 实现在 step 7。
//
// Init 内会执行：
//   - 加载配置（通过 pkg/config.Store）
//   - 初始化外部连接（Redis / MySQL / Kafka / etcd / 对象存储）
//   - 自检：Middleware 注册顺序（validateMiddlewareOrder）
//   - 自检：Adapter 覆盖（DB 中的 vendor 与 adapter.Vendors() 比对）
//   - 自检：Extractor 覆盖
//   - 启动后台任务（限流 oversell 计算 / 健康 prober / 计量事件 consumer 等）
package bootstrap
