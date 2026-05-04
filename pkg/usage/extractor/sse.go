package extractor

import "bytes"

// extractDataPayload 从 SSE 单行里拿 "data: " 后的 payload；非 data 行返回 nil。
//
// 兼容 SSE 规范的 "data: " 后零或一个空格 + 跳过末尾 \r。
func extractDataPayload(line []byte) []byte {
	const prefix = "data:"
	if !bytes.HasPrefix(line, []byte(prefix)) {
		return nil
	}
	rest := line[len(prefix):]
	if len(rest) > 0 && rest[0] == ' ' {
		rest = rest[1:]
	}
	return bytes.TrimSpace(rest)
}
