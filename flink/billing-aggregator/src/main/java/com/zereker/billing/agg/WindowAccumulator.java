package com.zereker.billing.agg;

import java.io.Serializable;
import java.time.Instant;
import java.util.HashMap;
import java.util.Map;

/**
 * Intermediate accumulator state used by {@link LineAggregateFunction}.
 *
 * <p>Map keyed by {@link LineKey}; {@code lastEndTime} drives the pricing query
 * timestamp in the downstream enrich operator (§6.1).
 */
public class WindowAccumulator implements Serializable {

    public final Map<LineKey, LineAggregate> lines = new HashMap<>();
    public long eventsConsumed;
    public Instant lastEndTime;

    public WindowAccumulator() {}
}
