package com.zereker.billing.domain;

import com.fasterxml.jackson.annotation.JsonProperty;

import java.io.Serializable;
import java.time.Instant;
import java.util.List;

/**
 * Output batch: one per (account_id, window). See docs/09 §7.
 *
 * <p>Idempotency key: {@code event_id = "agg_" + accountId + "_" + windowStart}.
 * Downstream MUST dedupe on this key.
 */
public class BillingBatch implements Serializable {

    @JsonProperty("schema_version")
    public String schemaVersion;

    @JsonProperty("event_id")
    public String eventId;

    @JsonProperty("window_start")
    public Instant windowStart;

    @JsonProperty("window_end")
    public Instant windowEnd;

    @JsonProperty("account_id")
    public String accountId;

    @JsonProperty("currency")
    public String currency;

    @JsonProperty("totals")
    public BillingTotals totals;

    @JsonProperty("lines")
    public List<BillingLine> lines;

    @JsonProperty("stats")
    public Stats stats;

    @JsonProperty("generated_at")
    public Instant generatedAt;

    public BillingBatch() {}

    /** Transparency counters; not used by downstream billing logic. */
    public static class Stats implements Serializable {
        @JsonProperty("events_consumed")
        public long eventsConsumed;

        @JsonProperty("events_late_dropped")
        public long eventsLateDropped;

        @JsonProperty("lines_enrich_failed")
        public long linesEnrichFailed;

        public Stats() {}
    }
}
