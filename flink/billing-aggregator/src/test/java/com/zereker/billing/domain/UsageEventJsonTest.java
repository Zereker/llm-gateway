package com.zereker.billing.domain;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.SerializationFeature;
import com.fasterxml.jackson.datatype.jsr310.JavaTimeModule;
import org.junit.jupiter.api.Test;

import static org.assertj.core.api.Assertions.assertThat;

/**
 * Confirms the envelope mirrors the Go-side {@code pkg/usage.UsageEvent} after
 * the request_id / trace_id de-duplication: only {@code schema_version, event_id,
 * usage, created_at} live at the top level.
 */
class UsageEventJsonTest {

    private static final ObjectMapper M = new ObjectMapper()
            .registerModule(new JavaTimeModule())
            .disable(SerializationFeature.WRITE_DATES_AS_TIMESTAMPS);

    @Test
    void decodes_envelope_matching_docs08_sample() throws Exception {
        String json = """
            {
              "schema_version": "usage.v1",
              "event_id": "01JABC",
              "usage": {
                "input": 128,
                "output": 256,
                "total": 384,
                "raw": {},
                "source": "upstream",
                "estimator": "",
                "confidence": "exact",
                "meta": {
                  "account_id": "acct_abc",
                  "sub_account_id": "sub_001",
                  "api_key_id": "ak_xxx",
                  "model": "gpt-4o",
                  "vendor": "openai",
                  "endpoint_id": "12345",
                  "service_id": "svc_gpt4o",
                  "model_service_id": 12345,
                  "service_update_time": "2026-04-18T09:00:00Z",
                  "request_id": "req_x",
                  "trace_id": "4bf9",
                  "start_time": "2026-05-16T09:59:57Z",
                  "end_time": "2026-05-16T10:00:00Z",
                  "ttft_ms": 320,
                  "total_latency": 2800
                }
              },
              "created_at": "2026-05-16T10:00:00Z"
            }
            """;

        UsageEvent ev = M.readValue(json, UsageEvent.class);

        assertThat(ev.schemaVersion).isEqualTo("usage.v1");
        assertThat(ev.eventId).isEqualTo("01JABC");
        assertThat(ev.usage.input).isEqualTo(128);
        assertThat(ev.usage.meta.accountId).isEqualTo("acct_abc");
        assertThat(ev.usage.meta.modelServiceId).isEqualTo(12345);
        assertThat(ev.usage.meta.serviceUpdateTime.toString()).isEqualTo("2026-04-18T09:00:00Z");
        assertThat(ev.usage.meta.endTime.toString()).isEqualTo("2026-05-16T10:00:00Z");
    }
}
