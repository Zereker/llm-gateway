package com.zereker.billing.source;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.SerializationFeature;
import com.fasterxml.jackson.datatype.jsr310.JavaTimeModule;
import com.zereker.billing.domain.UsageEvent;
import org.apache.flink.api.common.serialization.DeserializationSchema;
import org.apache.flink.api.common.typeinfo.TypeInformation;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import java.io.IOException;

/**
 * Deserializes Kafka {@code billing.usage.recorded.v1} JSON bytes to {@link UsageEvent}.
 *
 * <p>Failures (malformed JSON / missing required fields) MUST not blow up the pipeline.
 * The current implementation returns {@code null} which Flink's Kafka source skips —
 * a downstream side-output for DLQ should consume {@link UsageEvent#schemaVersion} == null
 * sentinels OR we should switch to a richer {@code KafkaRecordDeserializationSchema} that
 * routes raw bytes to a DLQ topic. Left as TODO(team).
 */
public class UsageEventDeserializer implements DeserializationSchema<UsageEvent> {

    private static final long serialVersionUID = 1L;
    private static final Logger LOG = LoggerFactory.getLogger(UsageEventDeserializer.class);

    private transient ObjectMapper mapper;

    @Override
    public UsageEvent deserialize(byte[] message) throws IOException {
        if (mapper == null) {
            mapper = new ObjectMapper()
                    .registerModule(new JavaTimeModule())
                    .disable(SerializationFeature.WRITE_DATES_AS_TIMESTAMPS);
        }
        try {
            UsageEvent ev = mapper.readValue(message, UsageEvent.class);
            if (ev == null || ev.usage == null || ev.usage.meta == null
                    || ev.usage.meta.accountId == null || ev.usage.meta.accountId.isEmpty()
                    || ev.usage.meta.endTime == null
                    || ev.eventId == null || ev.eventId.isEmpty()) {
                LOG.warn("dropping invalid usage event: missing required fields");
                return null;
            }
            return ev;
        } catch (Exception e) {
            LOG.warn("usage event deserialize failed: {}", e.toString());
            return null;
        }
    }

    @Override
    public boolean isEndOfStream(UsageEvent nextElement) {
        return false;
    }

    @Override
    public TypeInformation<UsageEvent> getProducedType() {
        return TypeInformation.of(UsageEvent.class);
    }
}
