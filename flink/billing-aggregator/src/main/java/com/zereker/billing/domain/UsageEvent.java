package com.zereker.billing.domain;

import com.fasterxml.jackson.annotation.JsonProperty;

import java.io.Serializable;
import java.time.Instant;

/**
 * Envelope of Kafka topic {@code billing.usage.recorded.v1} (docs/08 §5).
 *
 * <p>Fields:
 * <ul>
 *   <li>{@code schema_version}: currently "usage.v1"
 *   <li>{@code event_id}: producer-side idempotency key
 *   <li>{@code usage}: full usage payload (request_id / trace_id live inside Usage.meta)
 *   <li>{@code created_at}: outbox enqueue time (NOT request completion time;
 *       use {@code usage.meta.endTime} for event time)
 * </ul>
 */
public class UsageEvent implements Serializable {

    @JsonProperty("schema_version")
    public String schemaVersion;

    @JsonProperty("event_id")
    public String eventId;

    @JsonProperty("usage")
    public Usage usage;

    @JsonProperty("created_at")
    public Instant createdAt;

    public UsageEvent() {}
}
