// endpoint_scan.go：启动期 endpoint 配置完整性扫描（docs/00 §3 step 6）。
//
// 本仓库的唯一运维接口是裸 SQL INSERT——protocol 写错（'openai ' 尾空格）、
// vendor Factory 未注册、translator 缺失、routing.url 指向 cloud metadata 这类
// 错配**没有任何入库校验**。不扫描的话，坏行的表现是请求侧 503 "no candidates"
// 且没有任何日志指向它。
//
// 扫描只 warn + metric（llm_gateway_endpoint_misconfigured_total），**不阻塞启动**
// ——一行坏配置不该拖垮其它健康 endpoint 的服务。
package main

import (
	"context"
	"log/slog"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/invoker"
	"github.com/zereker/llm-gateway/pkg/metric"
	"github.com/zereker/llm-gateway/pkg/protocol"
	"github.com/zereker/llm-gateway/pkg/protocol/quirks"
	"github.com/zereker/llm-gateway/pkg/repo"
	"github.com/zereker/llm-gateway/pkg/translator"
)

// clientProtocols gateway 暴露的客户端入口协议（docs/02 §2）；translator 可达性
// 按"至少有一个客户端协议能到达该 endpoint"判定。
var clientProtocols = []domain.Protocol{
	domain.ProtoOpenAI,
	domain.ProtoAnthropic,
	domain.ProtoResponses,
}

// scanEndpoints 拉全部 endpoint 逐行校验。DB 错只 warn（启动期 DB 可用性已由
// Migrate + CheckSchema 把关；这里失败多半是竞态，不值得 fail）。
func scanEndpoints(ctx context.Context, reader repo.EndpointReader, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	eps, err := reader.List(ctx)
	if err != nil {
		log.Warn("endpoint scan: list failed; skipping startup validation", "err", err)
		return
	}

	bad := 0
	for _, row := range eps {
		ep := repo.ToDomainEndpoint(row)
		for _, reason := range validateEndpoint(ep) {
			bad++
			log.Warn("endpoint misconfigured",
				"endpoint_id", ep.ID, "name", ep.Name, "vendor", ep.Vendor,
				"protocol_raw", row.Protocol, "reason", reason)
			metric.Inc(metric.EndpointMisconfiguredTotal, "vendor", ep.Vendor, "reason", reason)
		}
	}
	log.Info("endpoint scan complete", "total", len(eps), "misconfigured_findings", bad)
}

// validateEndpoint 返回一行 endpoint 的全部错配原因（空 = 健康）。
func validateEndpoint(ep *domain.Endpoint) []string {
	var reasons []string

	// 1) protocol 合法性：ProtoUnknown = ParseProtocol 没认出来（typo / 尾空格）。
	//    这个 endpoint 会被 eligibility 静默剔除。
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

	// 5) quirks 可编译性：typo 字段在这里暴露，而不是请求侧 PhaseQuirks 报错。
	if len(ep.Quirks) > 0 {
		if _, err := quirks.CompileJSON(ep.Quirks); err != nil {
			reasons = append(reasons, "invalid_quirks_spec")
		}
	}

	return reasons
}

// validateRoutingURL 校验上游 URL；返回错配 reason（空 = 通过）。
//
// **SSRF 边界**（有意收窄）：只挡 **cloud metadata** 面（169.254.0.0/16 链路本地
// 段 + 知名 metadata 主机名）——那永远不是合法上游。**不挡私网 IP**：self-hosted
// vLLM / Ollama 部署在内网是本项目的一等场景（docs/00 §1）。
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
	// metadata IP（169.254.0.0/16 / fe80::/10 / AWS IMDSv6 fd00:ec2::254）——
	// 判定逻辑与 dial-time SSRF 防线共用 invoker.IsMetadataIP，单一真源。
	// 注意：这是启动期**预警**（warn + metric，不阻塞）；运行期真正的拦截在
	// invoker 拨号钩子（按解析后 IP，挡 DNS-rebinding）。
	if ip, err := netip.ParseAddr(host); err == nil && invoker.IsMetadataIP(ip) {
		return "metadata_endpoint"
	}
	return ""
}
