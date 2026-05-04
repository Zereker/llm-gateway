package middleware

import (
	"github.com/gin-gonic/gin"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
)

// WithSourceProtocol 把客户端协议钉在路由注册期。
//
// 路由本身告诉下游"这条路径=哪个协议"，Envelope 不再需要做 path 启发式匹配，
// 改名 / 加路径 / 多协议复用同一前缀都不会再因 Detector 误判出问题。
//
// 调用顺序：必须排在 Envelope 之前。本 middleware 在 RequestContext 上预创建
// 一个 RequestEnvelope shell（只填 SourceProtocol / Modality），Envelope 中间件
// 之后填 RawBytes / Parsed / RequestTime。
//
// proto == ProtoUnknown 视为非法（路由注册期就该明确）；不做兜底。
func WithSourceProtocol(proto domain.Protocol, mod domain.Modality) gin.HandlerFunc {
	return func(c *gin.Context) {
		rc := GetRequestContext(c)
		if rc.Envelope == nil {
			rc.Envelope = &domain.RequestEnvelope{}
		}
		rc.Envelope.SourceProtocol = proto
		rc.Envelope.Modality = mod
		c.Next()
	}
}
