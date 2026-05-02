package middleware

// Register 装配主链路 middleware。顺序固定，不可调整。
//
// 顺序（实际 gin.Use 调用顺序）：
//
//	M1 TraceContext → M9 Recover → M2 Auth → M3 Envelope → M4 Budget →
//	M5 ModelService → M6 Limit → M8 ContentModeration → M7 Schedule → M10 Tracing
//
// TODO: 完整实现（含 Deps struct 与 Validate 自检）在 step 7。
