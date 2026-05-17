package com.zereker.billing.agg;

import java.io.Serializable;
import java.util.Comparator;
import java.util.Objects;

/**
 * Granularity of a billing line: (subAccountId, model, vendor). See docs/09 §7.
 *
 * <p>Implements equals/hashCode for HashMap aggregation and Comparable for
 * deterministic line ordering (required for byte-level idempotent emit).
 */
public final class LineKey implements Serializable, Comparable<LineKey> {

    private static final Comparator<String> NULL_FIRST =
            Comparator.nullsFirst(Comparator.naturalOrder());

    public final String subAccountId;
    public final String model;
    public final String vendor;

    public LineKey(String subAccountId, String model, String vendor) {
        this.subAccountId = subAccountId;
        this.model = model;
        this.vendor = vendor;
    }

    @Override
    public boolean equals(Object o) {
        if (this == o) return true;
        if (!(o instanceof LineKey other)) return false;
        return Objects.equals(subAccountId, other.subAccountId)
                && Objects.equals(model, other.model)
                && Objects.equals(vendor, other.vendor);
    }

    @Override
    public int hashCode() {
        return Objects.hash(subAccountId, model, vendor);
    }

    @Override
    public int compareTo(LineKey o) {
        int c = NULL_FIRST.compare(subAccountId, o.subAccountId);
        if (c != 0) return c;
        c = NULL_FIRST.compare(model, o.model);
        if (c != 0) return c;
        return NULL_FIRST.compare(vendor, o.vendor);
    }
}
