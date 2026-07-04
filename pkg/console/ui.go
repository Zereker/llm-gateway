package console

import _ "embed"

// indexHTML 是控制面单文件 Web UI（Phase 3），编译期 embed 进 binary——零外部资源、
// 无构建步骤、跟 API 同一个进程同源提供（不引 CORS）。
//
// **有意 vanilla / 单文件**：这是运维后台不是消费级产品，一个自包含 HTML（内联
// CSS/JS）足够，且部署零依赖。真要做成对外 SaaS 控制台再上正经前端工程。
//
//go:embed ui/index.html
var indexHTML []byte
