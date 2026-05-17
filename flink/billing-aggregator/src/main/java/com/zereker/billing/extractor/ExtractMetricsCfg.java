package com.zereker.billing.extractor;

import java.io.Serializable;
import java.util.List;
import java.util.Map;

/**
 * Pure-data config carried into {@link ExtractMetricsFn} (which builds the actual
 * {@link ExtractorRegistry} in its {@code open()} so SpEL {@code Expression}
 * objects never have to round-trip through Flink's serializer).
 */
public class ExtractMetricsCfg implements Serializable {

    public final String nacosServer;
    public final String nacosNamespace;
    public final String nacosGroup;
    public final List<String> dataIds;
    public final Map<String, String> vendorToSpec;

    public ExtractMetricsCfg(String nacosServer, String nacosNamespace, String nacosGroup,
                             List<String> dataIds, Map<String, String> vendorToSpec) {
        this.nacosServer = nacosServer;
        this.nacosNamespace = nacosNamespace;
        this.nacosGroup = nacosGroup;
        this.dataIds = List.copyOf(dataIds);
        this.vendorToSpec = vendorToSpec == null ? Map.of() : Map.copyOf(vendorToSpec);
    }
}
