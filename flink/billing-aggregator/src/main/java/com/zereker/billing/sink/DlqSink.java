package com.zereker.billing.sink;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.SerializationFeature;
import com.fasterxml.jackson.datatype.jsr310.JavaTimeModule;
import com.zereker.billing.domain.BillingBatch;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

/**
 * DLQ output: late events, enrich failures, and primary-sink failures all funnel here.
 * Same JSON-per-line shape as {@link LogSink} but to a separate logger
 * ({@code billing.dlq}) so ops can alert on it independently.
 */
public class DlqSink implements BillingBatchSink {

    private static final long serialVersionUID = 1L;
    private static final Logger LOG = LoggerFactory.getLogger("billing.dlq");
    private static final ObjectMapper MAPPER = new ObjectMapper()
            .registerModule(new JavaTimeModule())
            .disable(SerializationFeature.WRITE_DATES_AS_TIMESTAMPS);

    @Override
    public String name() {
        return "dlq";
    }

    @Override
    public void emit(BillingBatch batch) throws Exception {
        LOG.warn("{}", MAPPER.writeValueAsString(batch));
    }
}
