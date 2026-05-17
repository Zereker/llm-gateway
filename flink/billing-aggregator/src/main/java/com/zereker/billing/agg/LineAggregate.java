package com.zereker.billing.agg;

import java.io.Serializable;
import java.time.Instant;
import java.util.HashMap;
import java.util.Map;

/**
 * Per-line accumulator: all numeric dimensions live in {@link #dimensions}, keyed
 * by metric strings from {@link com.zereker.billing.extractor.MetricKeys}. The
 * aggregator is therefore vendor-agnostic — pricing rules join on the same keys.
 *
 * <p>{@code modelServiceId} / {@code serviceUpdateTime} are pricing-lookup fingerprints
 * carried through; first non-null wins (within one LineKey they must be invariant).
 */
public class LineAggregate implements Serializable {

    public long requests;
    /** metric_key -> accumulated value (long; tokens / seconds / counts). */
    public final Map<String, Long> dimensions = new HashMap<>();

    public String serviceId;
    public long modelServiceId;
    public Instant serviceUpdateTime;

    public LineAggregate() {}

    public void addEvent(Map<String, Long> dims, String serviceId, long modelServiceId, Instant serviceUpdateTime) {
        this.requests += 1;
        if (dims != null) {
            for (Map.Entry<String, Long> e : dims.entrySet()) {
                dimensions.merge(e.getKey(), e.getValue(), Long::sum);
            }
        }
        if (this.serviceId == null) this.serviceId = serviceId;
        if (this.modelServiceId == 0L) this.modelServiceId = modelServiceId;
        if (this.serviceUpdateTime == null) this.serviceUpdateTime = serviceUpdateTime;
    }

    public void mergeFrom(LineAggregate other) {
        this.requests += other.requests;
        for (Map.Entry<String, Long> e : other.dimensions.entrySet()) {
            this.dimensions.merge(e.getKey(), e.getValue(), Long::sum);
        }
        if (this.serviceId == null) this.serviceId = other.serviceId;
        if (this.modelServiceId == 0L) this.modelServiceId = other.modelServiceId;
        if (this.serviceUpdateTime == null) this.serviceUpdateTime = other.serviceUpdateTime;
    }
}
