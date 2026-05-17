package com.zereker.billing.pricing;

import com.fasterxml.jackson.databind.JsonNode;
import com.zereker.billing.agg.LineAggregate;

import java.util.Iterator;
import java.util.Map;

/**
 * Pure {@code (LineAggregate, PricingRule) -> cost} (docs/09 §6.5).
 *
 * <p>Iterates every dimension in {@code agg.dimensions} and multiplies by the
 * matching {@code rates.<dim>} from {@code rule.rateJson}. Dimensions without a
 * matching rate contribute 0 (logged via the calling operator's metric, NOT
 * silently zeroed in this calculator — that's the caller's transparency duty).
 *
 * <p>Supported rule_json fields (admin-side authoritative):
 * <pre>{@code
 * {
 *   "version":  1,
 *   "currency": "CNY",
 *   "base_unit": "1K_tokens" | "1M_tokens" | "1_request" | "1_second" | "1_image",
 *   "rates":     { "<metric_key>": <unit_price>, ... },
 *   "model_ratio":   1.0,            // optional overall multiplier
 *   "tiered_prices": [               // optional; activates when a threshold dim exceeds
 *     { "threshold_dim": "input_tokens", "threshold": 100000,
 *       "rates": { "input_tokens": 0.004, "output_tokens": 0.015 } }
 *   ]
 * }
 * }</pre>
 */
public final class CostCalculator {

    private CostCalculator() {}

    public static Double calculate(LineAggregate agg, PricingRule rule) {
        if (rule == null || rule.rateJson == null || agg == null) return null;

        JsonNode rates = pickRates(rule.rateJson, agg);
        double divisor = baseUnitDivisor(rule.baseUnit);

        double cost = 0.0;
        for (Map.Entry<String, Long> dim : agg.dimensions.entrySet()) {
            JsonNode rateNode = rates.path(dim.getKey());
            if (!rateNode.isNumber()) continue;
            cost += dim.getValue() * rateNode.asDouble() / divisor;
        }

        double modelRatio = rule.rateJson.path("model_ratio").asDouble(1.0);
        return cost * modelRatio;
    }

    /**
     * Tiered selection: walk {@code tiered_prices[]} in declared order; first tier
     * whose {@code threshold_dim} is exceeded wins (its {@code rates} replace the
     * default {@code rates}). Empty {@code tiered_prices} → default rates.
     */
    private static JsonNode pickRates(JsonNode ruleJson, LineAggregate agg) {
        JsonNode tiers = ruleJson.path("tiered_prices");
        if (tiers.isArray() && tiers.size() > 0) {
            Iterator<JsonNode> it = tiers.elements();
            while (it.hasNext()) {
                JsonNode tier = it.next();
                String dim = tier.path("threshold_dim").asText("");
                long threshold = tier.path("threshold").asLong(0L);
                long value = agg.dimensions.getOrDefault(dim, 0L);
                if (!dim.isEmpty() && value > threshold) {
                    return tier.path("rates");
                }
            }
        }
        return ruleJson.path("rates");
    }

    private static double baseUnitDivisor(String baseUnit) {
        if (baseUnit == null) return 1_000_000.0;
        return switch (baseUnit) {
            case "1M_tokens" -> 1_000_000.0;
            case "1K_tokens" -> 1_000.0;
            case "1_request", "1_image", "1_second" -> 1.0;
            default -> 1_000_000.0;
        };
    }
}
