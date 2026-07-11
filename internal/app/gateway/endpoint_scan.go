// endpoint_scan.go: startup-time endpoint configuration integrity scan (docs/00 §3 step 6).
//
// This repo's only operational interface is raw SQL INSERT—misconfigurations
// like a mistyped protocol ('openai ' with a trailing space), an unregistered
// vendor Factory, a missing translator, or routing.url pointing at cloud
// metadata **have no insert-time validation at all**. Without this scan, a bad
// row manifests as a request-side 503 "no candidates" with no log pointing at
// the cause.
//
// The scan only warns + emits a metric (llm_gateway_endpoint_misconfigured_total),
// **it never blocks startup**—one bad config row shouldn't take down service for
// other healthy endpoints.
package gateway

import (
	"context"
	"log/slog"
	"time"

	"github.com/zereker/llm-gateway/pkg/endpointcheck"
	"github.com/zereker/llm-gateway/pkg/metric"
	"github.com/zereker/llm-gateway/pkg/repo"
)

// scanEndpoints fetches all endpoints and validates them row by row. A DB
// error here only warns (startup-time DB availability is already gated by
// Migrate + CheckSchema; a failure here is most likely a race and not worth
// failing over).
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
