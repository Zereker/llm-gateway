package com.zereker.billing.pricing;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.zereker.billing.agg.LineAggregate;
import com.zereker.billing.extractor.MetricKeys;
import org.junit.jupiter.api.Test;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.within;

class CostCalculatorTest {

    private static final ObjectMapper M = new ObjectMapper();

    /**
     * Real-world Claude Opus 4.7 case provided by product:
     * <pre>
     *   in              0.036  / 1K tokens
     *   out             0.180  / 1K tokens
     *   cache.read      0.0036 / 1K tokens
     *   5m-cache.write  0.045  / 1K tokens
     *   1h-cache.write  0.072  / 1K tokens
     * </pre>
     *
     * Sample event: 10000 input + 15000 output + 40000 cache_read + 2000 5m-write + 0 1h-write
     * Expected:
     *   10  * 0.036  = 0.36
     *   15  * 0.180  = 2.70
     *   40  * 0.0036 = 0.144
     *   2   * 0.045  = 0.09
     *   0   * 0.072  = 0
     *                  ------
     *                  3.294
     */
    @Test
    void claude_opus_4_7_five_dimensions() throws Exception {
        PricingRule rule = new PricingRule();
        rule.baseUnit = "1K_tokens";
        rule.currency = "CNY";
        rule.rateJson = M.readTree("""
            {
              "version": 1,
              "currency": "CNY",
              "base_unit": "1K_tokens",
              "rates": {
                "input_tokens":          0.036,
                "output_tokens":         0.180,
                "cache_read_tokens":     0.0036,
                "cache_5m_write_tokens": 0.045,
                "cache_1h_write_tokens": 0.072
              }
            }
            """);

        LineAggregate agg = new LineAggregate();
        agg.dimensions.put(MetricKeys.INPUT_TOKENS,         10_000L);
        agg.dimensions.put(MetricKeys.OUTPUT_TOKENS,        15_000L);
        agg.dimensions.put(MetricKeys.CACHE_READ_TOKENS,    40_000L);
        agg.dimensions.put(MetricKeys.CACHE_5M_WRITE_TOKENS, 2_000L);
        agg.dimensions.put(MetricKeys.CACHE_1H_WRITE_TOKENS,     0L);

        Double cost = CostCalculator.calculate(agg, rule);
        assertThat(cost).isCloseTo(3.294, within(1e-9));
    }

    @Test
    void model_ratio_multiplies() throws Exception {
        PricingRule rule = new PricingRule();
        rule.baseUnit = "1K_tokens";
        rule.rateJson = M.readTree("""
            { "version": 1, "currency": "USD", "base_unit": "1K_tokens",
              "rates": { "input_tokens": 1.0 },
              "model_ratio": 0.5 }
            """);
        LineAggregate agg = new LineAggregate();
        agg.dimensions.put(MetricKeys.INPUT_TOKENS, 1_000L);
        Double cost = CostCalculator.calculate(agg, rule);
        // 1000 / 1000 * 1.0 * 0.5 = 0.5
        assertThat(cost).isEqualTo(0.5);
    }

    @Test
    void tiered_prices_switch_when_threshold_exceeded() throws Exception {
        PricingRule rule = new PricingRule();
        rule.baseUnit = "1K_tokens";
        rule.rateJson = M.readTree("""
            { "version": 1, "currency": "USD", "base_unit": "1K_tokens",
              "rates": { "input_tokens": 0.005 },
              "tiered_prices": [
                { "threshold_dim": "input_tokens", "threshold": 100000,
                  "rates": { "input_tokens": 0.004 } }
              ] }
            """);
        // below threshold → base rate 0.005
        LineAggregate a = new LineAggregate();
        a.dimensions.put(MetricKeys.INPUT_TOKENS, 50_000L);
        assertThat(CostCalculator.calculate(a, rule)).isCloseTo(50.0 * 0.005, within(1e-9));

        // above threshold → tiered rate 0.004
        LineAggregate b = new LineAggregate();
        b.dimensions.put(MetricKeys.INPUT_TOKENS, 200_000L);
        assertThat(CostCalculator.calculate(b, rule)).isCloseTo(200.0 * 0.004, within(1e-9));
    }

    @Test
    void null_rule_returns_null() {
        assertThat(CostCalculator.calculate(new LineAggregate(), null)).isNull();
    }

    @Test
    void dimension_without_rate_contributes_zero() throws Exception {
        PricingRule rule = new PricingRule();
        rule.baseUnit = "1K_tokens";
        rule.rateJson = M.readTree("""
            { "version": 1, "currency": "USD", "base_unit": "1K_tokens",
              "rates": { "input_tokens": 0.005 } }
            """);
        LineAggregate agg = new LineAggregate();
        agg.dimensions.put(MetricKeys.INPUT_TOKENS, 1_000L);
        agg.dimensions.put(MetricKeys.IMAGE_INPUT_COUNT, 5L);  // no rate for this dim
        Double cost = CostCalculator.calculate(agg, rule);
        assertThat(cost).isCloseTo(0.005, within(1e-9));
    }
}
