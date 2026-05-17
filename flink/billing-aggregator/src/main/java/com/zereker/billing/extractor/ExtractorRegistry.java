package com.zereker.billing.extractor;

import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import java.io.Serializable;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;

/**
 * Process-wide registry of {@link CompiledExtractor}s keyed by spec name.
 *
 * <p>Loaded once at job start from {@code NacosConfigClient}; lookups are
 * lock-free. Hot reload (Nacos listener) replaces entries atomically.
 *
 * <p>{@code resolveSpecName(vendor)} is the single point where the aggregator
 * decides "which spec applies to this event" — by default the vendor string
 * itself is the spec name; {@code application.yaml#extractor.mapping} overrides.
 */
public class ExtractorRegistry implements Serializable {

    private static final Logger LOG = LoggerFactory.getLogger(ExtractorRegistry.class);

    private final ConcurrentHashMap<String, CompiledExtractor> byName = new ConcurrentHashMap<>();
    private final Map<String, String> vendorToSpec;

    public ExtractorRegistry(Map<String, String> vendorToSpec) {
        this.vendorToSpec = vendorToSpec == null ? Map.of() : Map.copyOf(vendorToSpec);
    }

    public void put(CompiledExtractor extractor) {
        if (extractor == null || extractor.name() == null) return;
        byName.put(extractor.name(), extractor);
        LOG.info("registered extractor spec: {}", extractor.name());
    }

    public CompiledExtractor getByVendor(String vendor) {
        if (vendor == null || vendor.isEmpty()) return null;
        String specName = vendorToSpec.getOrDefault(vendor, vendor);
        return byName.get(specName);
    }

    public int size() {
        return byName.size();
    }
}
