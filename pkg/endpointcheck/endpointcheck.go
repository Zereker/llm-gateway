// Package endpointcheck 是 endpoint 业务配置的合法性校验器——**控制面写入前**
// 和**数据面启动期扫描**共用同一份逻辑，保证"什么算错配"只有一处定义。
//
// 校验依赖 vendor Factory registry（protocol.LookupFactory）和 translator
// registry（translator.FindVia）——调用方的 main 必须已 blank-import 对应的
// vendor / translator 子包完成注册，否则合法 endpoint 会被误判 vendor_not_registered
// / no_translator_path。cmd/gateway 与 cmd/console 都带这批 blank import。
package endpointcheck

import (
	"net/netip"
	"net/url"
	"strings"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/invoker"
	"github.com/zereker/llm-gateway/pkg/protocol"
	"github.com/zereker/llm-gateway/pkg/protocol/quirks"
	"github.com/zereker/llm-gateway/pkg/translator"
)

// clientProtocols gateway 暴露的客户端入口协议（docs/02 §2）；translator 可达性
// 按"至少有一个客户端协议能到达该 endpoint"判定。
var clientProtocols = []domain.Protocol{
	domain.ProtoOpenAI,
	domain.ProtoAnthropic,
	domain.ProtoResponses,
}

// Validate 返回一行 endpoint 的全部错配原因（空 slice = 健康）。
//
// reason 是稳定的 snake_case 标识，既做 metric label 也做控制面 400 响应里的
// 机器可读错误码。
func Validate(ep *domain.Endpoint) []string {
	var reasons []string

	// 1) protocol 合法性：ProtoUnknown = ParseProtocol 没认出来（typo / 尾空格）。
	if ep.Protocol == domain.ProtoUnknown {
		reasons = append(reasons, "unknown_protocol")
	}

	// 2) vendor Factory 注册检查。
	if protocol.LookupFactory(ep.Vendor) == nil {
		reasons = append(reasons, "vendor_not_registered")
	}

	// 3) translator 可达性：任一客户端协议能到达（直连或 pivot 组合）即可。
	if ep.Protocol != domain.ProtoUnknown {
		reachable := false
		for _, src := range clientProtocols {
			if translator.FindVia(src, ep.Protocol, domain.ProtoOpenAI) != nil {
				reachable = true
				break
			}
		}
		if !reachable {
			reasons = append(reasons, "no_translator_path")
		}
	}

	// 4) routing.url 基本校验 + metadata SSRF 防线。
	if r := validateRoutingURL(ep.Routing.URL); r != "" {
		reasons = append(reasons, r)
	}

	// 5) quirks 可编译性：typo 字段在这里暴露，而不是请求侧 PhaseQuirks 才报错。
	if len(ep.Quirks) > 0 {
		if _, err := quirks.CompileJSON(ep.Quirks); err != nil {
			reasons = append(reasons, "invalid_quirks_spec")
		}
	}

	return reasons
}

// validateRoutingURL 校验上游 URL；返回错配 reason（空 = 通过）。
//
// **SSRF 边界**（有意收窄）：只挡 cloud metadata 面（169.254.0.0/16 / fe80::/10 /
// AWS IMDSv6 + 知名 metadata 主机名）——那永远不是合法上游。**不挡私网 IP**：
// self-hosted vLLM / Ollama 部署在内网是本项目一等场景（docs/00 §1）。这里是
// 启动期 / 写入期**预校验**；运行期真正的拦截在 invoker 拨号钩子（按解析后 IP，
// 挡 DNS-rebinding）。
func validateRoutingURL(raw string) string {
	if raw == "" {
		return "empty_routing_url"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "invalid_routing_url"
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "invalid_routing_scheme"
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "invalid_routing_url"
	}
	// 知名 metadata 主机名
	switch host {
	case "metadata.google.internal", "metadata", "instance-data":
		return "metadata_endpoint"
	}
	// metadata IP（判定与 dial-time SSRF 防线共用 invoker.IsMetadataIP，单一真源）
	if ip, err := netip.ParseAddr(host); err == nil && invoker.IsMetadataIP(ip) {
		return "metadata_endpoint"
	}
	return ""
}
