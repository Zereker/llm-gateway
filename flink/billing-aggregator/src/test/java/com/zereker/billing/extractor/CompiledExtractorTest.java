package com.zereker.billing.extractor;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import org.junit.jupiter.api.Test;
import org.yaml.snakeyaml.LoaderOptions;
import org.yaml.snakeyaml.Yaml;
import org.yaml.snakeyaml.constructor.Constructor;

import java.util.Map;

import static org.assertj.core.api.Assertions.assertThat;

/**
 * Verifies that an Anthropic-shaped raw usage JSON, processed by an extractor spec
 * loaded as YAML (mimicking Nacos), yields the 5 dimensions Claude Opus 4.7 needs.
 *
 * <p>The aggregator code is vendor-agnostic — the test here exercises only the
 * generic compile + run path with realistic vendor-specific SpEL written in YAML.
 */
class CompiledExtractorTest {

    private static final ObjectMapper M = new ObjectMapper();

    @Test
    void anthropic_five_dimensions_from_yaml_spec() throws Exception {
        // Nacos dataId: extractor-anthropic.yaml
        // Note: #path is a helper function (see JsonPathHelper); ?: is SpEL's Elvis fallback.
        String yaml = """
            name: anthropic
            metrics:
              input_tokens:
                expr: "#path(#root, 'input_tokens') ?: 0"
              output_tokens:
                expr: "#path(#root, 'output_tokens') ?: 0"
              cache_read_tokens:
                expr: "#path(#root, 'cache_read_input_tokens') ?: 0"
              cache_5m_write_tokens:
                expr: "#path(#root, 'cache_creation.ephemeral_5m_input_tokens') ?: 0"
              cache_1h_write_tokens:
                expr: "#path(#root, 'cache_creation.ephemeral_1h_input_tokens') ?: 0"
            """;
        Yaml y = new Yaml(new Constructor(ExtractorSpec.class, new LoaderOptions()));
        ExtractorSpec spec = y.load(yaml);
        CompiledExtractor extractor = CompiledExtractor.compile(spec);

        // Realistic Anthropic /v1/messages usage object
        String rawJson = """
            {
              "input_tokens": 10000,
              "output_tokens": 15000,
              "cache_read_input_tokens": 40000,
              "cache_creation": {
                "ephemeral_5m_input_tokens": 2000,
                "ephemeral_1h_input_tokens": 0
              }
            }
            """;
        JsonNode raw = M.readTree(rawJson);

        Map<String, Long> dims = extractor.extract(raw);
        assertThat(dims).containsEntry(MetricKeys.INPUT_TOKENS,          10_000L);
        assertThat(dims).containsEntry(MetricKeys.OUTPUT_TOKENS,         15_000L);
        assertThat(dims).containsEntry(MetricKeys.CACHE_READ_TOKENS,     40_000L);
        assertThat(dims).containsEntry(MetricKeys.CACHE_5M_WRITE_TOKENS,  2_000L);
        assertThat(dims).containsEntry(MetricKeys.CACHE_1H_WRITE_TOKENS,      0L);
    }

    @Test
    void openai_input_minus_cached() throws Exception {
        // OpenAI's input_tokens INCLUDES cached; pricing needs them split.
        String yaml = """
            name: openai_responses
            metrics:
              input_tokens:
                expr: "(#path(#root, 'input_tokens') ?: 0) - (#path(#root, 'input_tokens_details.cached_tokens') ?: 0)"
              cached_input_tokens:
                expr: "#path(#root, 'input_tokens_details.cached_tokens') ?: 0"
              output_tokens:
                expr: "#path(#root, 'output_tokens') ?: 0"
              reasoning_tokens:
                expr: "#path(#root, 'output_tokens_details.reasoning_tokens') ?: 0"
            """;
        Yaml y = new Yaml(new Constructor(ExtractorSpec.class, new LoaderOptions()));
        ExtractorSpec spec = y.load(yaml);
        CompiledExtractor extractor = CompiledExtractor.compile(spec);

        String rawJson = """
            {
              "input_tokens": 50000,
              "input_tokens_details": { "cached_tokens": 10000 },
              "output_tokens": 5000,
              "output_tokens_details": { "reasoning_tokens": 800 }
            }
            """;
        Map<String, Long> dims = extractor.extract(M.readTree(rawJson));
        assertThat(dims).containsEntry(MetricKeys.INPUT_TOKENS,        40_000L);
        assertThat(dims).containsEntry(MetricKeys.CACHED_INPUT_TOKENS, 10_000L);
        assertThat(dims).containsEntry(MetricKeys.OUTPUT_TOKENS,        5_000L);
        assertThat(dims).containsEntry(MetricKeys.REASONING_TOKENS,       800L);
    }

    @Test
    void null_raw_emits_zeros() throws Exception {
        String yaml = """
            name: anthropic
            metrics:
              input_tokens:
                expr: "#path(#root, 'input_tokens') ?: 0"
            """;
        Yaml y = new Yaml(new Constructor(ExtractorSpec.class, new LoaderOptions()));
        ExtractorSpec spec = y.load(yaml);
        CompiledExtractor extractor = CompiledExtractor.compile(spec);

        Map<String, Long> dims = extractor.extract(null);
        assertThat(dims).containsEntry(MetricKeys.INPUT_TOKENS, 0L);
    }

    @Test
    void compile_fails_fast_on_bad_spel() {
        ExtractorSpec spec = new ExtractorSpec();
        spec.name = "broken";
        MetricExpr m = new MetricExpr();
        m.expr = "this is ((( not valid SpEL";
        spec.metrics = Map.of("input_tokens", m);

        try {
            CompiledExtractor.compile(spec);
            org.assertj.core.api.Assertions.fail("expected IllegalArgumentException");
        } catch (IllegalArgumentException ok) {
            assertThat(ok.getMessage()).contains("broken").contains("input_tokens");
        }
    }
}
