package com.zereker.billing.extractor;

import java.io.Serializable;
import java.util.Map;

/**
 * YAML-side schema of a single extractor spec, sourced from Nacos.
 *
 * <p>One spec covers one upstream response format (e.g. all Claude models share
 * one {@code anthropic} spec). Lookup key is the {@code name} field; binding
 * usage events to specs happens via {@link ExtractorRegistry} using
 * {@code meta.vendor} (or an application.yaml override mapping).
 *
 * <p>YAML example (Nacos dataId {@code extractor-anthropic.yaml}):
 * <pre>{@code
 * name: anthropic
 * metrics:
 *   input_tokens:
 *     expr: "#root['input_tokens'].asLong()"
 *     default: 0
 *     required: true
 *   cache_read_tokens:
 *     expr: "#root['cache_read_input_tokens']?.asLong() ?: 0"
 *     default: 0
 *   cache_5m_write_tokens:
 *     expr: "#root['cache_creation']?['ephemeral_5m_input_tokens']?.asLong() ?: #root['cache_creation_input_tokens']?.asLong() ?: 0"
 *     default: 0
 *   cache_1h_write_tokens:
 *     expr: "#root['cache_creation']?['ephemeral_1h_input_tokens']?.asLong() ?: 0"
 *     default: 0
 *   output_tokens:
 *     expr: "#root['output_tokens'].asLong()"
 *     required: true
 * }</pre>
 */
public class ExtractorSpec implements Serializable {

    /** Logical name; equal to the Nacos dataId stem (minus {@code extractor-} prefix and {@code .yaml}). */
    public String name;

    /** metric_key -> expression. metric_key MUST be one of {@link MetricKeys}. */
    public Map<String, MetricExpr> metrics;

    public ExtractorSpec() {}
}
