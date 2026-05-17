package com.zereker.billing.sink;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.SerializationFeature;
import com.fasterxml.jackson.datatype.jsr310.JavaTimeModule;
import com.zereker.billing.domain.BillingBatch;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

/**
 * Default sink: write the batch as a JSON line on a dedicated SLF4J logger so log4j2
 * can route it to a rolling file (see {@code src/main/resources/log4j2.xml}, logger
 * name {@code billing.batches}).
 *
 * <p>This is intentionally minimal — for higher throughput / disk failure handling
 * production should swap in a sink that talks directly to the platform's log shipper.
 */
public class LogSink implements BillingBatchSink {

    private static final long serialVersionUID = 1L;
    private static final Logger LOG = LoggerFactory.getLogger("billing.batches");
    private static final ObjectMapper MAPPER = new ObjectMapper()
            .registerModule(new JavaTimeModule())
            .disable(SerializationFeature.WRITE_DATES_AS_TIMESTAMPS);

    @Override
    public String name() {
        return "log";
    }

    @Override
    public void emit(BillingBatch batch) throws Exception {
        LOG.info("{}", MAPPER.writeValueAsString(batch));
    }
}
