package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// genTraceID 生成形如 "tr_<16 hex>" 的 trace ID（64 bit 随机）。
//
// 客户端可通过 X-Trace-Id header 透传自己的 ID 覆盖；M1 仅在 header 缺失时调用本函数。
func genTraceID() string {
	return "tr_" + randHex(8)
}

// genRequestID 生成形如 "req_<12 hex>" 的请求 ID（48 bit 随机）。
//
// 同请求内唯一；不同请求间冲突概率可忽略（48 bit 在百万 QPS 量级下足够）。
func genRequestID() string {
	return "req_" + randHex(6)
}

// randHex 返回 byteLen 字节随机数据的 hex 字符串（长度 = 2 * byteLen）。
//
// crypto/rand 失败时退到 timestamp 兜底（极少发生，但避免 panic 让请求失败）。
func randHex(byteLen int) string {
	b := make([]byte, byteLen)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
