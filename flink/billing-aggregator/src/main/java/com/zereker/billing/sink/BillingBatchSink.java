package com.zereker.billing.sink;

import com.zereker.billing.domain.BillingBatch;

import java.io.Serializable;

/**
 * Pluggable output for a finished billing batch (docs/09 §9).
 *
 * <p>Implementations MUST be serializable (Flink ships them to task managers) and
 * idempotent on best effort: same {@code BillingBatch.eventId} may arrive twice
 * after checkpoint restore; downstream is responsible for final dedupe.
 *
 * <p>Built-in drivers: {@link LogSink}, {@link DlqSink}.
 * Future drivers (kafka / http) implement this interface and register via config.
 */
public interface BillingBatchSink extends Serializable, AutoCloseable {

    /** Driver name for metrics labels and log lines. */
    String name();

    /** Emit one batch. May throw; the pipeline's RichSinkFunction wrapper handles retry/DLQ. */
    void emit(BillingBatch batch) throws Exception;

    @Override
    default void close() throws Exception {
        // default: no-op
    }
}
