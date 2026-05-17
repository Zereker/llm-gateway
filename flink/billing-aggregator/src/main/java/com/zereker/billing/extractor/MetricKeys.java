package com.zereker.billing.extractor;

/**
 * Canonical metric-key dictionary. Both extractor specs (Nacos) and pricing rules
 * (admin {@code pricing_versions.rule_json}) use these names; cost lookup joins on
 * the exact string. Adding a new dimension MUST update both sides.
 *
 * <p>This is the only place a metric key string appears in code — extractor SpEL
 * lives in YAML, pricing rates live in JSON. The aggregator itself is dimension-agnostic.
 */
public final class MetricKeys {

    private MetricKeys() {}

    public static final String INPUT_TOKENS         = "input_tokens";
    public static final String OUTPUT_TOKENS        = "output_tokens";
    public static final String CACHED_INPUT_TOKENS  = "cached_input_tokens";
    public static final String CACHE_READ_TOKENS    = "cache_read_tokens";
    public static final String CACHE_5M_WRITE_TOKENS = "cache_5m_write_tokens";
    public static final String CACHE_1H_WRITE_TOKENS = "cache_1h_write_tokens";
    public static final String REASONING_TOKENS    = "reasoning_tokens";
    public static final String AUDIO_INPUT_SECONDS = "audio_input_seconds";
    public static final String AUDIO_OUTPUT_SECONDS = "audio_output_seconds";
    public static final String IMAGE_INPUT_COUNT   = "image_input_count";
    public static final String IMAGE_OUTPUT_COUNT  = "image_output_count";
    public static final String TEXT_CHAR_COUNT     = "text_char_count";
    public static final String REQUESTS            = "requests";
}
