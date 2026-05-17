package com.zereker.billing.domain;

import com.fasterxml.jackson.annotation.JsonIgnore;
import com.fasterxml.jackson.annotation.JsonProperty;

import java.io.Serializable;
import java.time.Instant;
import java.util.Map;

/**
 * One row of a billing batch (docs/09 §7).
 *
 * <p>Granularity: (sub_account_id, model, vendor). {@link #dimensions} holds every
 * metric the upstream extractor pulled out of {@code usage.raw} (keyed by the
 * canonical strings in {@code MetricKeys}). {@link #cost} is null iff
 * {@link #enrichmentFailed} is true — downstream MUST tolerate null.
 */
public class BillingLine implements Serializable {

    @JsonProperty("sub_account_id")
    public String subAccountId;

    @JsonProperty("model")
    public String model;

    @JsonProperty("vendor")
    public String vendor;

    @JsonProperty("service_id")
    public String serviceId;

    @JsonProperty("requests")
    public long requests;

    /** metric_key -> accumulated value within the window. */
    @JsonProperty("dimensions")
    public Map<String, Long> dimensions;

    @JsonProperty("cost")
    public Double cost;

    @JsonProperty("rule_class")
    public String ruleClass;

    @JsonProperty("enrichment_failed")
    public boolean enrichmentFailed;

    /**
     * Pricing fingerprint, carried between operators (Flink POJO serializer keeps it)
     * but hidden from the JSON output (@JsonIgnore). Not part of the public schema.
     */
    @JsonIgnore
    public long modelServiceId;

    @JsonIgnore
    public Instant serviceUpdateTime;

    public BillingLine() {}
}
