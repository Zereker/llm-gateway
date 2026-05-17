package com.zereker.billing.domain;

import com.fasterxml.jackson.annotation.JsonProperty;

import java.io.Serializable;
import java.time.Instant;

/**
 * Mirror of Go-side {@code pkg/domain.UsageMeta}; see docs/05 §4 and docs/09 §4.
 *
 * <p>POJO with public fields + no-arg constructor so Flink's POJO serializer can handle it
 * without falling back to Kryo. Jackson uses field-level annotations.
 */
public class UsageMeta implements Serializable {

    @JsonProperty("account_id")
    public String accountId;

    @JsonProperty("sub_account_id")
    public String subAccountId;

    @JsonProperty("api_key_id")
    public String apiKeyId;

    @JsonProperty("model")
    public String model;

    @JsonProperty("vendor")
    public String vendor;

    @JsonProperty("endpoint_id")
    public String endpointId;

    @JsonProperty("service_id")
    public String serviceId;

    @JsonProperty("model_service_id")
    public long modelServiceId;

    @JsonProperty("service_update_time")
    public Instant serviceUpdateTime;

    @JsonProperty("request_id")
    public String requestId;

    @JsonProperty("trace_id")
    public String traceId;

    @JsonProperty("start_time")
    public Instant startTime;

    @JsonProperty("end_time")
    public Instant endTime;

    @JsonProperty("ttft_ms")
    public long ttftMs;

    @JsonProperty("total_latency")
    public long totalLatency;

    public UsageMeta() {}
}
