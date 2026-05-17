package com.zereker.billing.extractor;

import com.zereker.billing.domain.UsageEvent;

import java.io.Serializable;
import java.util.Map;

/**
 * UsageEvent + extracted dimensions, produced by {@link ExtractMetricsFn} just
 * before {@code keyBy(accountId)}. Aggregation downstream sees only this type
 * and never has to touch raw usage JSON again.
 *
 * <p>{@code dimensions} is null when extraction failed (no spec registered for
 * the vendor / extractor evaluation errored across the board); the aggregator
 * treats null as "skip this event for pricing", still increments {@code requests}.
 */
public class EnrichedEvent implements Serializable {

    public UsageEvent event;
    /** metric_key -> long count; keys defined in {@link MetricKeys}. */
    public Map<String, Long> dimensions;

    public EnrichedEvent() {}

    public EnrichedEvent(UsageEvent event, Map<String, Long> dimensions) {
        this.event = event;
        this.dimensions = dimensions;
    }
}
