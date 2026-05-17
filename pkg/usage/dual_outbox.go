package usage

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"github.com/zereker/llm-gateway/pkg/metric"
)

// DualWriteOutbox 把同一个 OutboxEvent 写到两个 sink：
//
//   - file (sync, source of truth)——写成功就算 commit
//   - kafka (async best-effort)——提供低延迟广播给计费/对账/配额 consumer
//
// 这是 Transactional Outbox Pattern 的实现：file 是真理，Kafka 是镜像。broker
// 挂了仍然能 commit；事件落地不依赖 Kafka 健康。后续由外部 replay 工具读
// file 把缺的事件补发到 Kafka（consumer 侧按 event_id 幂等去重）。
//
// 跟 AsyncKafkaOutbox + DLQ 的对比：
//
//   - AsyncKafkaOutbox + DLQ：主 topic 失败转写 DLQ topic；但 DLQ 跟主 topic 在
//     同一个 broker 集群上，broker 整个挂了 DLQ 也失败。
//   - DualWriteOutbox：file 在本地磁盘，broker 故障域跟 disk 故障域独立；除非
//     gateway 进程所在机器的盘满 / 损坏，否则不会丢数据。
//
// 详见 docs/architecture/05-metering-billing.md §5（usage outbox）。
type DualWriteOutbox struct {
	file  OutboxPublisher
	kafka OutboxPublisher
	log   *slog.Logger
}

// NewDualWriteOutbox 用现成的 file + kafka publisher 组合。
//
// 调用方负责构造各子 publisher（典型：FileOutbox + AsyncKafkaOutbox）。
// Close 只会关闭 file 句柄；kafka producer 的生命周期由 pkg/server 统一管理，
// 不在本类型内关闭——避免双关。
//
// log == nil 时用 slog.Default()。
func NewDualWriteOutbox(file, kafka OutboxPublisher, log *slog.Logger) *DualWriteOutbox {
	if log == nil {
		log = slog.Default()
	}
	return &DualWriteOutbox{file: file, kafka: kafka, log: log}
}

// Publish 实现 OutboxPublisher.Publish。
//
// 流程：
//  1. file.Publish (sync, blocking)——durability commit
//  2. kafka.Publish (async, best-effort)——失败不影响返回值
//
// **file ok + kafka 失败**：返回 nil（事件已落地；kafka 失败由 replay 工具补）。
// 仅记录 warn 日志 + outbox_kafka_publish_error metric。
//
// **file 失败**：仍然尝试 kafka，给一线希望（双失败概率极低）。返回 file 的错误
// 让 caller 知道数据可能丢——M10 不会因此阻塞业务响应，但会进 usage.publish.error
// 计数 + 告警。
func (d *DualWriteOutbox) Publish(ctx context.Context, evt *OutboxEvent) error {
	if evt == nil {
		return errors.New("usage: DualWriteOutbox.Publish: nil event")
	}
	if fileErr := d.file.Publish(ctx, evt); fileErr != nil {
		metric.Inc(metric.OutboxFileErrorTotal, "result", "error")
		d.log.WarnContext(ctx, "usage_events: file sink publish failed; data may be lost if kafka also fails",
			"event_key", evt.Key, "err", fileErr.Error())
		// 仍然尝试 kafka，给一线希望
		_ = d.kafka.Publish(ctx, evt)
		return fileErr
	}
	if err := d.kafka.Publish(ctx, evt); err != nil {
		metric.Inc(metric.OutboxKafkaPublishErrorTotal, "result", "error")
		d.log.WarnContext(ctx, "usage_events: kafka sink publish failed; file has source of truth",
			"event_key", evt.Key, "err", err.Error())
	}
	return nil
}

// Close 关闭 file sink；kafka producer 由 srv 统一管理。
func (d *DualWriteOutbox) Close() error {
	if c, ok := d.file.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

var (
	_ OutboxPublisher = (*DualWriteOutbox)(nil)
	_ io.Closer       = (*DualWriteOutbox)(nil)
)
