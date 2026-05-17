package com.zereker.billing.extractor;

import com.alibaba.nacos.api.NacosFactory;
import com.alibaba.nacos.api.config.ConfigService;
import com.alibaba.nacos.api.config.listener.Listener;
import com.alibaba.nacos.api.exception.NacosException;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.yaml.snakeyaml.Yaml;
import org.yaml.snakeyaml.constructor.Constructor;

import java.io.Closeable;
import java.io.IOException;
import java.util.Collection;
import java.util.Properties;
import java.util.concurrent.Executor;
import java.util.concurrent.Executors;

/**
 * Pulls extractor specs from Nacos at startup, optionally watches updates.
 *
 * <p>Each dataId follows the pattern {@code extractor-<name>.yaml} (group is
 * configurable, default {@code DEFAULT_GROUP}). The {@code name} inside the YAML
 * MUST match the dataId stem so registry lookups stay consistent.
 *
 * <p>Hot reload: each loaded dataId is wrapped with a {@link Listener}. When Nacos
 * pushes a new revision, the spec is re-compiled and atomically swapped in the
 * registry. Compilation failures keep the old spec in place + log a WARN.
 */
public class NacosConfigClient implements Closeable {

    private static final Logger LOG = LoggerFactory.getLogger(NacosConfigClient.class);

    private final ConfigService configService;
    private final String group;
    private final Executor listenerExec;

    public NacosConfigClient(String serverAddr, String namespace, String group) throws NacosException {
        Properties props = new Properties();
        props.put("serverAddr", serverAddr);
        if (namespace != null && !namespace.isEmpty()) {
            props.put("namespace", namespace);
        }
        this.configService = NacosFactory.createConfigService(props);
        this.group = group == null || group.isEmpty() ? "DEFAULT_GROUP" : group;
        this.listenerExec = Executors.newSingleThreadExecutor(r -> {
            Thread t = new Thread(r, "nacos-extractor-listener");
            t.setDaemon(true);
            return t;
        });
    }

    /**
     * Pulls every dataId provided, compiles into {@link CompiledExtractor},
     * registers in {@code registry}, and starts a Nacos listener on each.
     */
    public void loadAndWatch(Collection<String> dataIds, ExtractorRegistry registry) throws NacosException {
        for (String dataId : dataIds) {
            String yaml = configService.getConfig(dataId, group, 5_000L);
            if (yaml == null || yaml.isBlank()) {
                LOG.warn("nacos dataId {} empty — skipping", dataId);
                continue;
            }
            CompiledExtractor compiled = compileYaml(yaml, dataId);
            if (compiled != null) registry.put(compiled);

            configService.addListener(dataId, group, new Listener() {
                @Override public Executor getExecutor() { return listenerExec; }
                @Override
                public void receiveConfigInfo(String newYaml) {
                    LOG.info("nacos config change: dataId={}, len={}", dataId, newYaml == null ? 0 : newYaml.length());
                    CompiledExtractor newSpec = compileYaml(newYaml, dataId);
                    if (newSpec != null) registry.put(newSpec);
                }
            });
        }
    }

    private static CompiledExtractor compileYaml(String yaml, String dataId) {
        try {
            // Avoid SnakeYAML auto-class instantiation; build via constructor with allowed types.
            Yaml y = new Yaml(new Constructor(ExtractorSpec.class, new org.yaml.snakeyaml.LoaderOptions()));
            ExtractorSpec spec = y.load(yaml);
            return CompiledExtractor.compile(spec);
        } catch (Exception e) {
            LOG.warn("compile extractor spec failed (dataId={}): {}", dataId, e.toString());
            return null;
        }
    }

    @Override
    public void close() throws IOException {
        try {
            if (configService != null) configService.shutDown();
        } catch (NacosException e) {
            throw new IOException(e);
        }
    }
}
