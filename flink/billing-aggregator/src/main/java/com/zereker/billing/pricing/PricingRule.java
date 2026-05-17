package com.zereker.billing.pricing;

import com.fasterxml.jackson.databind.JsonNode;

import java.io.Serializable;

/**
 * Resolved pricing rule for one (account, model_service, rule_class) at a point in time.
 *
 * <p>Currently a thin wrapper around the JSON {@code rule_json} stored in
 * {@code pricing_versions}; the canonical schema is admin-side (see docs/05 §6 and
 * docs/09 §6.5). {@link CostCalculator} branches on {@code rateJson} fields.
 */
public class PricingRule implements Serializable {

    /** Version of rule_json schema; admin-side bumps when adding new dimensions. */
    public int version;

    /** ISO-4217 code, e.g. "USD". One plan / batch MUST use a single currency. */
    public String currency;

    /** "1M_tokens" | "1K_tokens" | "1_request" | "1_second" | "1_image"; see docs/05 §5.1. */
    public String baseUnit;

    /**
     * Raw rule body kept as JsonNode so admin-side can evolve the schema without
     * forcing a flink recompile. {@link CostCalculator} reads numeric rates from it.
     */
    public JsonNode rateJson;

    /**
     * Snapshot of pricing_versions.effective_from for the row that produced this rule.
     * Audit only; not used in current cost math.
     */
    public java.time.Instant effectiveFrom;

    public PricingRule() {}
}
