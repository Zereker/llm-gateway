// Package handler 定义 gin Handler 极简版本（约 30 行 / 模态）。
//
// 所有横切关注点已由 middleware 链处理；Handler 几乎只剩 Adapter.Run 调用。
//
// TODO: 各 modality handler（chat / message / image / embedding / tts / task）
// 在 step 5+ 加。
package handler
