package domain

// Adapter 是 RequestContext 字段所需的最小 Adapter 接口。
//
// 完整接口在 pkg/adapter.Adapter；实现完整接口即自动满足本接口。
// 拆为两层是为了避免 ctx → adapter 循环依赖
// （adapter 包要 import ctx，所以 ctx 不能反过来 import adapter）。
type Adapter interface {
	Vendor() string
	NewResponseSession() ResponseSession
}

// ResponseSession 处理上游响应（流式 / 非流式统一）。
type ResponseSession interface {
	Feed(chunk []byte) ([]byte, error)
	Finalize() (*Usage, *CanonicalResponse, *AdapterError)
}
