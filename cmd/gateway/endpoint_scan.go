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
	"time"

	"github.com/zereker/llm-gateway/pkg/endpointcheck"
	"github.com/zereker/llm-gateway/pkg/metric"
	"github.com/zereker/llm-gateway/pkg/repo"
)

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
		for _, reason := range endpointcheck.Validate(ep) {
			bad++
			log.Warn("endpoint misconfigured",
				"endpoint_id", ep.ID, "name", ep.Name, "vendor", ep.Vendor,
				"protocol_raw", row.Protocol, "reason", reason)
			metric.Inc(metric.EndpointMisconfiguredTotal, "vendor", ep.Vendor, "reason", reason)
		}
	}
	log.Info("endpoint scan complete", "total", len(eps), "misconfigured_findings", bad)
}
