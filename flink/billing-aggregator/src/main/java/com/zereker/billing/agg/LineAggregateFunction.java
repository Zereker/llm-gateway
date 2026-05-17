package com.zereker.billing.agg;

import com.zereker.billing.domain.UsageMeta;
import com.zereker.billing.extractor.EnrichedEvent;
import org.apache.flink.api.common.functions.AggregateFunction;

/**
 * Pure tumbling-window accumulator (docs/09 §5). Consumes {@link EnrichedEvent}
 * so extraction already happened upstream — no SpEL / IO inside the aggregate.
 */
public class LineAggregateFunction
        implements AggregateFunction<EnrichedEvent, WindowAccumulator, WindowAccumulator> {

    @Override
    public WindowAccumulator createAccumulator() {
        return new WindowAccumulator();
    }

    @Override
    public WindowAccumulator add(EnrichedEvent enriched, WindowAccumulator acc) {
        if (enriched == null || enriched.event == null
                || enriched.event.usage == null || enriched.event.usage.meta == null) {
            return acc;
        }
        UsageMeta meta = enriched.event.usage.meta;
        LineKey key = new LineKey(
                meta.subAccountId == null || meta.subAccountId.isEmpty() ? "_default" : meta.subAccountId,
                meta.model == null || meta.model.isEmpty() ? "_unknown" : meta.model,
                meta.vendor == null || meta.vendor.isEmpty() ? "_unknown" : meta.vendor
        );
        LineAggregate agg = acc.lines.computeIfAbsent(key, k -> new LineAggregate());
        agg.addEvent(enriched.dimensions, meta.serviceId, meta.modelServiceId, meta.serviceUpdateTime);

        acc.eventsConsumed += 1;
        if (meta.endTime != null && (acc.lastEndTime == null || meta.endTime.isAfter(acc.lastEndTime))) {
            acc.lastEndTime = meta.endTime;
        }
        return acc;
    }

    @Override
    public WindowAccumulator getResult(WindowAccumulator acc) {
        return acc;
    }

    @Override
    public WindowAccumulator merge(WindowAccumulator a, WindowAccumulator b) {
        b.lines.forEach((key, value) ->
                a.lines.merge(key, value, (x, y) -> { x.mergeFrom(y); return x; }));
        a.eventsConsumed += b.eventsConsumed;
        if (b.lastEndTime != null && (a.lastEndTime == null || b.lastEndTime.isAfter(a.lastEndTime))) {
            a.lastEndTime = b.lastEndTime;
        }
        return a;
    }
}
