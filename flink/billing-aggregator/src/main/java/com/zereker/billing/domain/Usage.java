package com.zereker.billing.domain;

import com.fasterxml.jackson.annotation.JsonProperty;
import com.fasterxml.jackson.databind.JsonNode;

import java.io.Serializable;

/**
 * Mirror of Go-side {@code pkg/domain.Usage}; see docs/05 §3.
 *
 * <p>{@code raw} is the upstream provider's original usage object; aggregator does not
 * interpret it. Stays as {@link JsonNode} (or null) so we keep round-trip fidelity.
 */
public class Usage implements Serializable {

    @JsonProperty("input")
    public long input;

    @JsonProperty("output")
    public long output;

    @JsonProperty("total")
    public long total;

    @JsonProperty("truncated")
    public boolean truncated;

    @JsonProperty("raw")
    public JsonNode raw;

    @JsonProperty("source")
    public String source;

    @JsonProperty("estimator")
    public String estimator;

    @JsonProperty("confidence")
    public String confidence;

    @JsonProperty("meta")
    public UsageMeta meta;

    public Usage() {}
}
