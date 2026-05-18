package com.zereker.billing;

import com.zereker.billing.agg.LineAggregate;
import com.zereker.billing.agg.LineAggregateFunction;
import com.zereker.billing.agg.LineKey;
import com.zereker.billing.agg.WindowAccumulator;
import com.zereker.billing.domain.BillingBatch;
import com.zereker.billing.domain.BillingLine;
import com.zereker.billing.domain.BillingTotals;
import com.zereker.billing.domain.UsageEvent;
import com.zereker.billing.extractor.EnrichedEvent;
import com.zereker.billing.extractor.ExtractMetricsCfg;
import com.zereker.billing.extractor.ExtractMetricsFn;
import com.zereker.billing.pricing.CostCalculator;
import com.zereker.billing.pricing.PricingResolver;
import com.zereker.billing.pricing.PricingRule;
import com.zereker.billing.sink.BillingBatchSink;
import com.zereker.billing.sink.DlqSink;
import com.zereker.billing.sink.LogSink;
import com.zereker.billing.source.UsageEventDeserializer;
import org.apache.flink.api.common.eventtime.WatermarkStrategy;
import org.apache.flink.api.common.functions.OpenContext;
import org.apache.flink.api.common.functions.RichFlatMapFunction;
import org.apache.flink.api.common.serialization.DeserializationSchema;
import org.apache.flink.connector.kafka.source.KafkaSource;
import org.apache.flink.connector.kafka.source.enumerator.initializer.OffsetsInitializer;
import org.apache.flink.streaming.api.CheckpointingMode;
import org.apache.flink.streaming.api.datastream.DataStream;
import org.apache.flink.streaming.api.environment.StreamExecutionEnvironment;
import org.apache.flink.streaming.api.functions.sink.SinkFunction;
import org.apache.flink.streaming.api.functions.windowing.ProcessWindowFunction;
import org.apache.flink.streaming.api.windowing.assigners.TumblingEventTimeWindows;
import org.apache.flink.streaming.api.windowing.time.Time;
import org.apache.flink.streaming.api.windowing.windows.TimeWindow;
import org.apache.flink.util.Collector;
import org.apache.flink.util.OutputTag;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import java.time.Duration;
import java.time.Instant;
import java.time.format.DateTimeFormatter;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

/**
 * Flink job entry point. Pipeline (docs/09 §5):
 *
 * <pre>
 * Kafka(billing.usage.recorded.v1) -> deserialize -> watermark(end_time)
 *  -> ExtractMetricsFn (Nacos-driven SpEL) -> EnrichedEvent
 *  -> keyBy(account_id) -> tumbling event-time window
 *  -> aggregate(LineAggregateFunction, ToBatchProcessFn)  // pure; no IO, no SpEL
 *  -> RichFlatMap EnrichFn   // PricingResolver (JDBC + Caffeine), CostCalculator
 *  -> Sink (LogSink by default; DLQ side output for enrich failures / late events)
 * </pre>
 *
 * <p>Wiring is intentionally hard-coded for v0.1; swap to YAML-driven assembly in v0.2.
 */
public class Main {

    private static final Logger LOG = LoggerFactory.getLogger(Main.class);

    /** Late event side output: type matches the windowed stream element (EnrichedEvent here). */
    public static final OutputTag<EnrichedEvent> LATE_EVENTS_TAG =
            new OutputTag<>("late-events") {};

    public static void main(String[] args) throws Exception {
        AppConfig cfg = AppConfig.hardcoded();

        StreamExecutionEnvironment env = StreamExecutionEnvironment.getExecutionEnvironment();
        env.setParallelism(cfg.parallelism);
        env.enableCheckpointing(cfg.checkpointIntervalMs, CheckpointingMode.EXACTLY_ONCE);
        env.getCheckpointConfig().setCheckpointTimeout(cfg.checkpointTimeoutMs);

        // ---- Source ----
        DeserializationSchema<UsageEvent> deser = new UsageEventDeserializer();
        KafkaSource<UsageEvent> source = KafkaSource.<UsageEvent>builder()
                .setBootstrapServers(cfg.bootstrapServers)
                .setTopics(cfg.topic)
                .setGroupId(cfg.consumerGroupId)
                .setStartingOffsets(OffsetsInitializer.earliest())
                .setValueOnlyDeserializer(deser)
                .build();

        WatermarkStrategy<UsageEvent> watermarks = WatermarkStrategy
                .<UsageEvent>forBoundedOutOfOrderness(Duration.ofSeconds(cfg.outOfOrdernessSeconds))
                .withTimestampAssigner((ev, ts) -> ev.usage.meta.endTime.toEpochMilli())
                .withIdleness(Duration.ofSeconds(cfg.idlenessSeconds));

        DataStream<UsageEvent> events = env.fromSource(source, watermarks, "usage-events");

        // ---- Extract metrics (SpEL specs from Nacos) ----
        DataStream<EnrichedEvent> enrichedEvents = events
                .map(new ExtractMetricsFn(cfg.extract))
                .name("extract-metrics");

        // ---- Window aggregate ----
        DataStream<BillingBatch> unpriced = enrichedEvents
                .keyBy(e -> e.event.usage.meta.accountId)
                .window(TumblingEventTimeWindows.of(Time.minutes(cfg.windowMinutes)))
                .allowedLateness(Time.minutes(cfg.allowedLatenessMinutes))
                .sideOutputLateData(LATE_EVENTS_TAG)
                .aggregate(new LineAggregateFunction(), new ToBatchProcessFn());

        // TODO(team): keep the SingleOutputStreamOperator handle from aggregate()
        //   and route .getSideOutput(LATE_EVENTS_TAG) → DlqSink via a small map.

        // ---- Pricing enrichment ----
        DataStream<BillingBatch> priced = unpriced
                .flatMap(new EnrichFn(cfg.pricing))
                .name("pricing-enrich");

        // ---- Sink ----
        priced.addSink(new EmitSink(new LogSink(), new DlqSink())).name("primary-sink");

        env.execute("billing-aggregator");
    }

    // -------- ProcessWindowFunction: WindowAccumulator -> BillingBatch (no cost yet) --------

    static class ToBatchProcessFn
            extends ProcessWindowFunction<WindowAccumulator, BillingBatch, String, TimeWindow> {

        @Override
        public void process(String accountId,
                            ProcessWindowFunction<WindowAccumulator, BillingBatch, String, TimeWindow>.Context ctx,
                            Iterable<WindowAccumulator> elements,
                            Collector<BillingBatch> out) {
            WindowAccumulator acc = elements.iterator().next();

            BillingBatch batch = new BillingBatch();
            batch.schemaVersion = "billing-batch.v1";
            batch.windowStart = Instant.ofEpochMilli(ctx.window().getStart());
            batch.windowEnd = Instant.ofEpochMilli(ctx.window().getEnd());
            batch.accountId = accountId;
            batch.eventId = "agg_" + accountId + "_" + DateTimeFormatter.ISO_INSTANT
                    .format(batch.windowStart).replaceAll("[:.\\-]", "");

            // deterministic line order — byte-level idempotent emit (docs/09 §7)
            List<Map.Entry<LineKey, LineAggregate>> sorted = new ArrayList<>(acc.lines.entrySet());
            sorted.sort(Map.Entry.comparingByKey());

            List<BillingLine> lines = new ArrayList<>(sorted.size());
            Map<String, Long> totalsDims = new HashMap<>();
            long totalRequests = 0;
            for (Map.Entry<LineKey, LineAggregate> e : sorted) {
                LineKey k = e.getKey();
                LineAggregate a = e.getValue();
                BillingLine line = new BillingLine();
                line.subAccountId = k.subAccountId;
                line.model = k.model;
                line.vendor = k.vendor;
                line.serviceId = a.serviceId;
                line.requests = a.requests;
                line.dimensions = new HashMap<>(a.dimensions);
                line.modelServiceId = a.modelServiceId;
                line.serviceUpdateTime = a.serviceUpdateTime;
                line.cost = null;          // filled by EnrichFn
                line.ruleClass = null;     // filled by EnrichFn
                line.enrichmentFailed = false;
                lines.add(line);

                totalRequests += a.requests;
                for (Map.Entry<String, Long> d : a.dimensions.entrySet()) {
                    totalsDims.merge(d.getKey(), d.getValue(), Long::sum);
                }
            }
            batch.lines = lines;

            BillingTotals totals = new BillingTotals();
            totals.requests = totalRequests;
            totals.dimensions = totalsDims;
            totals.cost = null;
            batch.totals = totals;

            BillingBatch.Stats stats = new BillingBatch.Stats();
            stats.eventsConsumed = acc.eventsConsumed;
            batch.stats = stats;

            batch.generatedAt = Instant.now();
            out.collect(batch);
        }
    }

    // -------- Pricing enrichment --------

    static class EnrichFn extends RichFlatMapFunction<BillingBatch, BillingBatch> {
        private static final long serialVersionUID = 1L;
        private final PricingResolverConfig cfg;
        private transient PricingResolver resolver;

        EnrichFn(PricingResolverConfig cfg) { this.cfg = cfg; }

        @Override
        public void open(OpenContext openContext) {
            this.resolver = new PricingResolver(
                    cfg.jdbcUrl, cfg.username, cfg.password,
                    cfg.poolMaxSize, cfg.cacheMaxSize, cfg.cacheTtl, cfg.dbSemaphorePermits);
            resolver.init();
        }

        @Override
        public void flatMap(BillingBatch batch, Collector<BillingBatch> out) {
            // Point-in-time pricing: use window_end as the query time. Cost stays
            // deterministic on replay and ≈ the actual completion time of the
            // events in the window (the bounded-out-of-orderness is ≤ 1min).
            Instant at = batch.windowEnd;
            String currency = null;
            double totalCost = 0;
            long failed = 0;

            for (BillingLine line : batch.lines) {
                if (line.modelServiceId == 0L) {
                    line.cost = null;
                    line.enrichmentFailed = true;
                    line.ruleClass = cfg.ruleClass;
                    failed++;
                    continue;
                }
                PricingRule rule = resolver.lookup(batch.accountId, line.modelServiceId, cfg.ruleClass, at);
                if (rule == null) {
                    line.cost = null;
                    line.enrichmentFailed = true;
                    line.ruleClass = cfg.ruleClass;
                    failed++;
                    continue;
                }
                line.ruleClass = cfg.ruleClass;

                // CostCalculator is pure: build a tiny aggregate-shaped view over line dims.
                LineAggregate view = new LineAggregate();
                view.requests = line.requests;
                view.dimensions.putAll(line.dimensions);

                Double cost = CostCalculator.calculate(view, rule);
                line.cost = cost;
                if (cost != null) totalCost += cost;

                if (currency == null) currency = rule.currency;
                else if (!currency.equals(rule.currency)) {
                    currency = "MIXED";
                    line.enrichmentFailed = true;
                    failed++;
                }
            }

            batch.currency = currency == null ? "USD" : currency;
            batch.totals.cost = (failed == batch.lines.size()) ? null : totalCost;
            batch.stats.linesEnrichFailed = failed;
            out.collect(batch);
        }

        @Override
        public void close() throws Exception {
            if (resolver != null) resolver.close();
        }
    }

    // -------- Sink wrapper --------

    static class EmitSink implements SinkFunction<BillingBatch> {
        private static final long serialVersionUID = 1L;
        private final BillingBatchSink primary;
        private final BillingBatchSink dlq;

        EmitSink(BillingBatchSink primary, BillingBatchSink dlq) {
            this.primary = primary;
            this.dlq = dlq;
        }

        @Override
        public void invoke(BillingBatch batch, Context ctx) throws Exception {
            primary.emit(batch);
            if (batch.stats != null && batch.stats.linesEnrichFailed > 0) {
                dlq.emit(batch);
            }
        }
    }

    // -------- Config (hard-coded; replace with YAML loader) --------

    record PricingResolverConfig(
            String jdbcUrl, String username, String password,
            int poolMaxSize, long cacheMaxSize, Duration cacheTtl, int dbSemaphorePermits,
            String ruleClass) implements java.io.Serializable {}

    record AppConfig(
            int parallelism,
            long checkpointIntervalMs,
            long checkpointTimeoutMs,
            String bootstrapServers,
            String topic,
            String consumerGroupId,
            int windowMinutes,
            int allowedLatenessMinutes,
            int outOfOrdernessSeconds,
            int idlenessSeconds,
            PricingResolverConfig pricing,
            ExtractMetricsCfg extract) {

        /**
         * Hard-coded defaults overridable via env vars (for E2E smoke + container deploys).
         * v0.2 will move to YAML loader (configs/local + cfg.yaml mount).
         *
         * <pre>
         * KAFKA_BOOTSTRAP_SERVERS  (default kafka-broker-0:9092)
         * KAFKA_TOPIC              (default billing.usage.recorded.v1)
         * CONSUMER_GROUP_ID        (default billing-aggregator)
         * WINDOW_MINUTES           (default 60; e2e set to 1)
         * ALLOWED_LATENESS_MINUTES (default 10)
         * PRICING_JDBC_URL         (default jdbc:mysql://admin-db:3306/llm_gateway?...)
         * PRICING_DB_USER          (default billing)
         * PRICING_DB_PASSWORD      (default empty)
         * NACOS_SERVER             (default nacos:8848)
         * NACOS_NAMESPACE          (default empty)
         * NACOS_GROUP              (default DEFAULT_GROUP)
         * </pre>
         */
        static AppConfig hardcoded() {
            return new AppConfig(
                    4,
                    60_000L,
                    300_000L,
                    env("KAFKA_BOOTSTRAP_SERVERS", "kafka-broker-0:9092"),
                    env("KAFKA_TOPIC", "billing.usage.recorded.v1"),
                    env("CONSUMER_GROUP_ID", "billing-aggregator"),
                    Integer.parseInt(env("WINDOW_MINUTES", "60")),
                    Integer.parseInt(env("ALLOWED_LATENESS_MINUTES", "10")),
                    Integer.parseInt(env("OUT_OF_ORDERNESS_SECONDS", "60")),
                    Integer.parseInt(env("IDLENESS_SECONDS", "300")),
                    new PricingResolverConfig(
                            env("PRICING_JDBC_URL",
                                    "jdbc:mysql://admin-db:3306/llm_gateway?useUnicode=true&characterEncoding=UTF-8"),
                            env("PRICING_DB_USER", "billing"),
                            env("PRICING_DB_PASSWORD", ""),
                            16, 50_000L, Duration.ofHours(1), 16,
                            "standard"),
                    new ExtractMetricsCfg(
                            env("NACOS_SERVER", "nacos:8848"),
                            env("NACOS_NAMESPACE", ""),
                            env("NACOS_GROUP", "DEFAULT_GROUP"),
                            List.of(
                                    "extractor-anthropic.yaml",
                                    "extractor-openai-responses.yaml",
                                    "extractor-google-gemini.yaml"),
                            Map.of(/* vendor-string → spec-name overrides; empty = use vendor name itself */)));
        }

        private static String env(String key, String dflt) {
            return System.getenv().getOrDefault(key, dflt);
        }
    }
}
