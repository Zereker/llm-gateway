package com.zereker.billing.extractor;

import java.io.Serializable;

/**
 * One {@code metric -> SpEL expression} definition in an {@link ExtractorSpec}.
 *
 * <p>Field intentionally minimal: write fallbacks inline in the expression with
 * {@code ?: 0} or similar. {@code default} / {@code required} were dropped because
 * (a) {@code default} is a Java reserved word that bites SnakeYAML and (b) the
 * inline-fallback style mirrors docs/05 long-form §14 (expr-lang's {@code ?? 0}).
 */
public class MetricExpr implements Serializable {

    /** SpEL expression; compiled once in {@link CompiledExtractor}. */
    public String expr;

    public MetricExpr() {}
}
