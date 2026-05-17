package com.zereker.billing.agg;

import com.zereker.billing.domain.Usage;
import com.zereker.billing.domain.UsageEvent;
import com.zereker.billing.domain.UsageMeta;
import com.zereker.billing.extractor.EnrichedEvent;
import com.zereker.billing.extractor.MetricKeys;
import org.junit.jupiter.api.Test;

import java.time.Instant;
import java.util.HashMap;
import java.util.Map;

import static org.assertj.core.api.Assertions.assertThat;

class LineAggregateFunctionTest {

    @Test
    void accumulates_same_line_dimensions() {
        LineAggregateFunction fn = new LineAggregateFunction();
        WindowAccumulator acc = fn.createAccumulator();

        fn.add(enriched("acct_a", "sub_1", "claude-opus-4-7", "anthropic",
                Map.of(MetricKeys.INPUT_TOKENS, 100L, MetricKeys.OUTPUT_TOKENS, 200L)), acc);
        fn.add(enriched("acct_a", "sub_1", "claude-opus-4-7", "anthropic",
                Map.of(MetricKeys.INPUT_TOKENS, 50L, MetricKeys.OUTPUT_TOKENS, 100L,
                        MetricKeys.CACHE_READ_TOKENS, 800L)), acc);

        assertThat(acc.lines).hasSize(1);
        LineKey key = new LineKey("sub_1", "claude-opus-4-7", "anthropic");
        LineAggregate agg = acc.lines.get(key);
        assertThat(agg.requests).isEqualTo(2);
        assertThat(agg.dimensions).containsEntry(MetricKeys.INPUT_TOKENS, 150L);
        assertThat(agg.dimensions).containsEntry(MetricKeys.OUTPUT_TOKENS, 300L);
        assertThat(agg.dimensions).containsEntry(MetricKeys.CACHE_READ_TOKENS, 800L);
        assertThat(acc.eventsConsumed).isEqualTo(2);
    }

    @Test
    void splits_distinct_keys_into_separate_lines() {
        LineAggregateFunction fn = new LineAggregateFunction();
        WindowAccumulator acc = fn.createAccumulator();
        fn.add(enriched("acct_a", "sub_1", "claude-opus-4-7", "anthropic",
                Map.of(MetricKeys.INPUT_TOKENS, 10L)), acc);
        fn.add(enriched("acct_a", "sub_1", "gpt-4o",          "openai",
                Map.of(MetricKeys.INPUT_TOKENS, 30L)), acc);
        fn.add(enriched("acct_a", "sub_2", "claude-opus-4-7", "anthropic",
                Map.of(MetricKeys.INPUT_TOKENS, 50L)), acc);

        assertThat(acc.lines).hasSize(3);
    }

    @Test
    void merge_two_accumulators_combines_dimensions() {
        LineAggregateFunction fn = new LineAggregateFunction();
        WindowAccumulator a = fn.createAccumulator();
        WindowAccumulator b = fn.createAccumulator();

        fn.add(enriched("acct_a", "sub_1", "claude-opus-4-7", "anthropic",
                Map.of(MetricKeys.INPUT_TOKENS, 100L)), a);
        fn.add(enriched("acct_a", "sub_1", "claude-opus-4-7", "anthropic",
                Map.of(MetricKeys.INPUT_TOKENS, 50L)), b);

        WindowAccumulator merged = fn.merge(a, b);
        LineKey key = new LineKey("sub_1", "claude-opus-4-7", "anthropic");
        assertThat(merged.lines.get(key).requests).isEqualTo(2);
        assertThat(merged.lines.get(key).dimensions).containsEntry(MetricKeys.INPUT_TOKENS, 150L);
    }

    @Test
    void missing_sub_account_falls_back_to_default() {
        LineAggregateFunction fn = new LineAggregateFunction();
        WindowAccumulator acc = fn.createAccumulator();
        fn.add(enriched("acct_a", null, "claude-opus-4-7", "anthropic", Map.of()), acc);
        assertThat(acc.lines).containsOnlyKeys(new LineKey("_default", "claude-opus-4-7", "anthropic"));
    }

    @Test
    void null_dimensions_still_counts_request() {
        LineAggregateFunction fn = new LineAggregateFunction();
        WindowAccumulator acc = fn.createAccumulator();
        EnrichedEvent e = enriched("acct_a", "sub_1", "x", "vendor_y", null);
        e.dimensions = null;
        fn.add(e, acc);
        LineKey key = new LineKey("sub_1", "x", "vendor_y");
        assertThat(acc.lines.get(key).requests).isEqualTo(1);
        assertThat(acc.lines.get(key).dimensions).isEmpty();
    }

    private static EnrichedEvent enriched(String acct, String sub, String model, String vendor,
                                          Map<String, Long> dims) {
        UsageEvent ev = new UsageEvent();
        ev.eventId = "evt_" + System.nanoTime();
        ev.schemaVersion = "usage.v1";
        ev.createdAt = Instant.now();
        ev.usage = new Usage();
        ev.usage.meta = new UsageMeta();
        ev.usage.meta.accountId = acct;
        ev.usage.meta.subAccountId = sub;
        ev.usage.meta.model = model;
        ev.usage.meta.vendor = vendor;
        ev.usage.meta.serviceId = "svc_" + model;
        ev.usage.meta.modelServiceId = 12345L;
        ev.usage.meta.serviceUpdateTime = Instant.parse("2026-04-18T09:00:00Z");
        ev.usage.meta.endTime = Instant.now();
        return new EnrichedEvent(ev, dims == null ? null : new HashMap<>(dims));
    }
}
