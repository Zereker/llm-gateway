package com.zereker.billing.pricing;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.github.benmanes.caffeine.cache.Cache;
import com.github.benmanes.caffeine.cache.Caffeine;
import com.zaxxer.hikari.HikariConfig;
import com.zaxxer.hikari.HikariDataSource;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import java.io.Closeable;
import java.io.IOException;
import java.io.Serializable;
import java.sql.Connection;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.Timestamp;
import java.time.Duration;
import java.time.Instant;
import java.util.Objects;
import java.util.concurrent.Semaphore;

/**
 * Resolves {@link PricingRule} from {@code pricing_versions} (docs/09 §6).
 *
 * <p>Construction is cheap; resources (Hikari pool) are lazily initialised on first
 * lookup so this class can be field-serialised by Flink's POJO serializer and the
 * pool is created in the operator's open() callback.
 *
 * <p>TODO(team): implement the §6.2 fallback path for legacy events missing
 * model_service_id; the current implementation requires a non-zero modelServiceId.
 */
public class PricingResolver implements Closeable, Serializable {

    private static final Logger LOG = LoggerFactory.getLogger(PricingResolver.class);

    private static final ObjectMapper MAPPER = new ObjectMapper();

    /** SQL is intentionally inlined; align field order with idx_active_lookup. */
    private static final String LOOKUP_SQL =
            "SELECT id, rule_json, effective_from " +
            "FROM pricing_versions " +
            "WHERE account_id = ? AND model_service_id = ? AND rule_class = ? " +
            "  AND effective_from <= ? " +
            "  AND (effective_to IS NULL OR effective_to > ?) " +
            "ORDER BY effective_from DESC LIMIT 1";

    private final String jdbcUrl;
    private final String username;
    private final String password;
    private final int poolMaxSize;
    private final long cacheMaxSize;
    private final Duration cacheTtl;
    private final int dbSemaphorePermits;

    // transient — set up in init(), torn down in close()
    private transient HikariDataSource ds;
    private transient Cache<CacheKey, PricingRule> cache;
    private transient Semaphore dbSemaphore;

    public PricingResolver(String jdbcUrl, String username, String password,
                           int poolMaxSize, long cacheMaxSize, Duration cacheTtl,
                           int dbSemaphorePermits) {
        this.jdbcUrl = jdbcUrl;
        this.username = username;
        this.password = password;
        this.poolMaxSize = poolMaxSize;
        this.cacheMaxSize = cacheMaxSize;
        this.cacheTtl = cacheTtl;
        this.dbSemaphorePermits = dbSemaphorePermits;
    }

    /** Invoke from RichFunction.open(). Idempotent. */
    public synchronized void init() {
        if (ds != null) return;
        HikariConfig cfg = new HikariConfig();
        cfg.setJdbcUrl(jdbcUrl);
        cfg.setUsername(username);
        cfg.setPassword(password);
        cfg.setMaximumPoolSize(poolMaxSize);
        cfg.setConnectionTimeout(5_000L);
        cfg.setPoolName("billing-aggregator-pricing");
        // Flink user-jar 通过自定义 classloader 加载，DriverManager 走 SPI 时只看
        // system classloader，找不到 fat jar 里的 com.mysql.cj.jdbc.Driver。
        // 显式指定 driverClassName 绕开 SPI 探测。
        cfg.setDriverClassName("com.mysql.cj.jdbc.Driver");
        this.ds = new HikariDataSource(cfg);
        this.cache = Caffeine.newBuilder()
                .maximumSize(cacheMaxSize)
                .expireAfterWrite(cacheTtl)
                .build();
        this.dbSemaphore = new Semaphore(dbSemaphorePermits, true);
        LOG.info("PricingResolver initialised: pool={}, cache={}/{}",
                poolMaxSize, cacheMaxSize, cacheTtl);
    }

    /**
     * Returns the active pricing rule, or {@code null} if not found / DB unreachable.
     * Caller treats null as enrichment_failed (docs/09 §6.4).
     */
    public PricingRule lookup(String accountId, long modelServiceId, String ruleClass, Instant at) {
        if (ds == null) {
            throw new IllegalStateException("PricingResolver.init() not called");
        }
        if (modelServiceId == 0L || accountId == null || at == null) {
            return null;
        }
        CacheKey key = new CacheKey(accountId, modelServiceId, ruleClass, at.truncatedTo(java.time.temporal.ChronoUnit.HOURS));
        PricingRule cached = cache.getIfPresent(key);
        if (cached != null) return cached;

        try {
            dbSemaphore.acquire();
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            return null;
        }
        try {
            PricingRule rule = queryWithRetry(accountId, modelServiceId, ruleClass, at, 3);
            if (rule != null) cache.put(key, rule);
            return rule;
        } finally {
            dbSemaphore.release();
        }
    }

    private PricingRule queryWithRetry(String accountId, long modelServiceId, String ruleClass,
                                       Instant at, int maxAttempts) {
        long backoffMs = 100;
        for (int attempt = 1; attempt <= maxAttempts; attempt++) {
            try (Connection conn = ds.getConnection();
                 PreparedStatement ps = conn.prepareStatement(LOOKUP_SQL)) {
                ps.setString(1, accountId);
                ps.setLong(2, modelServiceId);
                ps.setString(3, ruleClass);
                Timestamp ts = Timestamp.from(at);
                ps.setTimestamp(4, ts);
                ps.setTimestamp(5, ts);
                try (ResultSet rs = ps.executeQuery()) {
                    if (!rs.next()) return null;
                    PricingRule rule = new PricingRule();
                    String json = rs.getString("rule_json");
                    rule.rateJson = MAPPER.readTree(json);
                    rule.version = rule.rateJson.path("version").asInt(1);
                    rule.currency = rule.rateJson.path("currency").asText("USD");
                    rule.baseUnit = rule.rateJson.path("base_unit").asText("1M_tokens");
                    Timestamp ef = rs.getTimestamp("effective_from");
                    rule.effectiveFrom = ef == null ? null : ef.toInstant();
                    return rule;
                }
            } catch (Exception e) {
                LOG.warn("pricing lookup attempt {}/{} failed for ({}, {}): {}",
                        attempt, maxAttempts, accountId, modelServiceId, e.toString());
                if (attempt == maxAttempts) return null;
                try {
                    Thread.sleep(backoffMs);
                } catch (InterruptedException ie) {
                    Thread.currentThread().interrupt();
                    return null;
                }
                backoffMs = Math.min(backoffMs * 2, 2_000L);
            }
        }
        return null;
    }

    @Override
    public void close() throws IOException {
        if (ds != null) {
            ds.close();
            ds = null;
        }
    }

    /** Cache key truncated to hour so end_times within the same hour share an entry. */
    private record CacheKey(String accountId, long modelServiceId, String ruleClass, Instant hour) {
        @Override
        public boolean equals(Object o) {
            if (this == o) return true;
            if (!(o instanceof CacheKey other)) return false;
            return modelServiceId == other.modelServiceId
                    && Objects.equals(accountId, other.accountId)
                    && Objects.equals(ruleClass, other.ruleClass)
                    && Objects.equals(hour, other.hour);
        }
        @Override
        public int hashCode() {
            return Objects.hash(accountId, modelServiceId, ruleClass, hour);
        }
    }
}
