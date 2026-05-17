package com.zereker.billing.extractor;

import com.alibaba.nacos.api.exception.NacosException;
import com.zereker.billing.domain.UsageEvent;
import org.apache.flink.api.common.functions.OpenContext;
import org.apache.flink.api.common.functions.RichMapFunction;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import java.util.Map;

/**
 * Pulls extractor specs from Nacos on {@code open()}, evaluates them per record
 * against {@code usage.raw}. Pushes extraction off the AggregateFunction so SpEL
 * Expression objects (non-serializable) never enter Flink's checkpoint barrier.
 *
 * <p>If no spec registered for the event's vendor — the record is still forwarded,
 * but {@code dimensions} stays null so the aggregator's pricing step can mark
 * the line as {@code enrichment_failed} and route it to DLQ.
 */
public class ExtractMetricsFn extends RichMapFunction<UsageEvent, EnrichedEvent> {

    private static final long serialVersionUID = 1L;
    private static final Logger LOG = LoggerFactory.getLogger(ExtractMetricsFn.class);

    private final ExtractMetricsCfg cfg;
    private transient ExtractorRegistry registry;
    private transient NacosConfigClient client;

    public ExtractMetricsFn(ExtractMetricsCfg cfg) {
        this.cfg = cfg;
    }

    @Override
    public void open(OpenContext openContext) throws Exception {
        this.registry = new ExtractorRegistry(cfg.vendorToSpec);
        this.client = new NacosConfigClient(cfg.nacosServer, cfg.nacosNamespace, cfg.nacosGroup);
        try {
            client.loadAndWatch(cfg.dataIds, registry);
        } catch (NacosException e) {
            LOG.error("nacos load failed; aggregator will run with empty registry", e);
        }
        LOG.info("extract-metrics open: specs registered = {}", registry.size());
    }

    @Override
    public EnrichedEvent map(UsageEvent ev) {
        if (ev == null || ev.usage == null || ev.usage.meta == null) {
            return new EnrichedEvent(ev, null);
        }
        CompiledExtractor extractor = registry.getByVendor(ev.usage.meta.vendor);
        if (extractor == null) {
            return new EnrichedEvent(ev, null);
        }
        Map<String, Long> dims = extractor.extract(ev.usage.raw);
        return new EnrichedEvent(ev, dims);
    }

    @Override
    public void close() throws Exception {
        if (client != null) client.close();
    }
}
