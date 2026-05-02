package metric

// Inc 增加 1 次计数。
//
// v0.1 实现是 NoOp；生产 Prometheus / OTel 实现在 v0.5+ 引入。
// 保留函数签名是为了让所有 middleware 实现可以无条件调用，不必各自加 nil 检查。
func Inc(_ string, _ ...string) {}

// Observe 记录一次观测值（histogram）。v0.1 NoOp。
func Observe(_ string, _ float64, _ ...string) {}

// Gauge 设置一个 gauge 值。v0.1 NoOp。
func Gauge(_ string, _ float64, _ ...string) {}
