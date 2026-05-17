package com.zereker.billing.extractor;

import java.util.Map;

/**
 * SpEL helper exposed as the {@code #path} function (registered in
 * {@link CompiledExtractor}). Mirrors docs/05 §14's {@code path("a.b.c")} idiom.
 *
 * <p>{@code root} is whatever the spec author bound as {@code #root} — in this
 * job that's always {@code Map<String, Object>} (the upstream usage JSON parsed
 * by Jackson). Missing keys / non-map intermediaries return {@code null} so
 * spec authors can chain with the Elvis operator: {@code #path(#root, 'a.b.c') ?: 0}.
 */
public final class JsonPathHelper {

    private JsonPathHelper() {}

    /** Walks dotted path through nested Maps; returns null on any miss / type mismatch. */
    public static Object path(Object root, String dotted) {
        if (root == null || dotted == null || dotted.isEmpty()) return null;
        Object cur = root;
        int i = 0;
        while (i < dotted.length()) {
            int dot = dotted.indexOf('.', i);
            String seg = (dot < 0) ? dotted.substring(i) : dotted.substring(i, dot);
            if (cur instanceof Map<?, ?> m) {
                cur = m.get(seg);
                if (cur == null) return null;
            } else {
                return null;
            }
            i = (dot < 0) ? dotted.length() : dot + 1;
        }
        return cur;
    }
}
