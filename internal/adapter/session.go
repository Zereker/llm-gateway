package adapter

import (
	"github.com/zereker-labs/ai-gateway/internal/envelope"
	"github.com/zereker-labs/ai-gateway/internal/errs"
	"github.com/zereker-labs/ai-gateway/internal/usage"
)

// ResponseSession 处理上游响应（流式 / 非流式统一）。
//
// 非流式调用：
//
//	sess.Feed(fullBody)
//	u, resp, err := sess.Finalize()
//
// 流式调用：
//
//	for chunk := range upstream {
//	    out, err := sess.Feed(chunk)
//	    writer.Write(out)            // 实时写给客户端
//	}
//	u, resp, err := sess.Finalize()
//
// out 是"翻译 / 加工后写给客户端的字节"；非流式时通常返回空。
type ResponseSession interface {
	Feed(chunk []byte) ([]byte, error)
	Finalize() (*usage.Usage, *envelope.CanonicalResponse, *errs.Error)
}
