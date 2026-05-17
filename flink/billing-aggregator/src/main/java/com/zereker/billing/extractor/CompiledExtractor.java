package com.zereker.billing.extractor;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.expression.Expression;
import org.springframework.expression.ExpressionParser;
import org.springframework.expression.spel.standard.SpelExpressionParser;
import org.springframework.expression.spel.support.StandardEvaluationContext;

import java.io.Serializable;
import java.lang.reflect.Method;
import java.util.HashMap;
import java.util.LinkedHashMap;
import java.util.Map;

/**
 * Pre-compiled {@link ExtractorSpec}. SpEL parse happens once at construction;
 * the request path runs each {@link Expression#getValue} which is sub-millisecond.
 *
 * <p>Root binding: the raw upstream usage JSON is converted to
 * {@code Map<String, Object>} so SpEL's map indexer {@code #root['key']} works
 * naturally. The {@code #path(#root, 'a.b.c')} helper is registered as a SpEL
 * function — spec authors use it for nullable nested navigation, mirroring
 * docs/05 §14's {@code path("a.b.c")} idiom.
 *
 * <p>SECURITY NOTE: uses {@link StandardEvaluationContext} which allows method
 * invocation — fine for trusted Nacos-managed specs, NOT fine if specs ever
 * come from end users. TODO(team): add a SandboxedEvaluationContext if/when
 * the spec source becomes untrusted.
 */
public class CompiledExtractor implements Serializable {

    private static final Logger LOG = LoggerFactory.getLogger(CompiledExtractor.class);

    private static final ExpressionParser PARSER = new SpelExpressionParser();
    private static final ObjectMapper MAPPER = new ObjectMapper();

    private static final Method PATH_FN;
    static {
        try {
            PATH_FN = JsonPathHelper.class.getDeclaredMethod("path", Object.class, String.class);
        } catch (NoSuchMethodException e) {
            throw new ExceptionInInitializerError(e);
        }
    }

    private final String name;
    private final Map<String, Expression> compiled;

    private CompiledExtractor(String name, Map<String, Expression> compiled) {
        this.name = name;
        this.compiled = compiled;
    }

    /**
     * Compiles every expression up-front; throws if any one fails so the operator
     * fails fast at job start instead of mid-stream.
     */
    public static CompiledExtractor compile(ExtractorSpec spec) {
        if (spec == null || spec.name == null || spec.metrics == null) {
            throw new IllegalArgumentException("invalid extractor spec");
        }
        Map<String, Expression> compiled = new LinkedHashMap<>(spec.metrics.size());
        for (Map.Entry<String, MetricExpr> e : spec.metrics.entrySet()) {
            try {
                compiled.put(e.getKey(), PARSER.parseExpression(e.getValue().expr));
            } catch (Exception parseErr) {
                throw new IllegalArgumentException(
                        "spec " + spec.name + " metric " + e.getKey() + " parse failed: " + parseErr.getMessage(),
                        parseErr);
            }
        }
        return new CompiledExtractor(spec.name, compiled);
    }

    public String name() {
        return name;
    }

    /**
     * Apply every metric expression against the raw usage subtree. Errors are
     * downgraded to 0 — vendor responses are NOT our invariants.
     */
    @SuppressWarnings("unchecked")
    public Map<String, Long> extract(JsonNode raw) {
        Map<String, Long> out = new HashMap<>(compiled.size());
        Map<String, Object> rootMap = (raw == null || raw.isNull())
                ? Map.of()
                : MAPPER.convertValue(raw, Map.class);

        StandardEvaluationContext ctx = new StandardEvaluationContext(rootMap);
        ctx.registerFunction("path", PATH_FN);

        for (Map.Entry<String, Expression> e : compiled.entrySet()) {
            try {
                Object v = e.getValue().getValue(ctx);
                out.put(e.getKey(), asLong(v));
            } catch (Exception err) {
                LOG.warn("spec={} metric={} eval failed: {}", name, e.getKey(), err.toString());
                out.put(e.getKey(), 0L);
            }
        }
        return out;
    }

    private static long asLong(Object v) {
        if (v == null) return 0L;
        if (v instanceof Number n) return n.longValue();
        if (v instanceof String s) {
            try { return Long.parseLong(s); } catch (NumberFormatException ignored) { return 0L; }
        }
        return 0L;
    }
}
