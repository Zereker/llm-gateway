package com.zereker.billing.domain;

import com.fasterxml.jackson.annotation.JsonProperty;

import java.io.Serializable;
import java.util.Map;

/**
 * Account-level totals in a {@link BillingBatch}. Mirrors {@link BillingLine#dimensions}
 * shape; {@code cost} is sum of line costs (null when every line failed).
 */
public class BillingTotals implements Serializable {

    @JsonProperty("requests")
    public long requests;

    @JsonProperty("dimensions")
    public Map<String, Long> dimensions;

    @JsonProperty("cost")
    public Double cost;

    public BillingTotals() {}
}
