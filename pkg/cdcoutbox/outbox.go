// Package cdcoutbox 实现 transactional outbox 模式：
//
// admin 写 MySQL 时**同事务**追加一行到 data_change_outbox 表；
// 一个 background goroutine（OutboxRelay）周期性消费未发送的行，
// 把变更广播到 Redis（SET 实体缓存 key + PUBLISH 失效通知）。
//
// 这是给 gateway 三层缓存（L1 local LRU + L2 Redis + L3 MySQL fallback）
// 准实时刷新的入口。
//
// 详见 docs/06 §2 + 设计讨论（CDC 替代轮询/共享 DB 直读）。
package cdcoutbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/redis/go-redis/v9"
)

// Op 变更类型。
type Op string

const (
	OpUpsert Op = "upsert"
	OpDelete Op = "delete"
)

// Change 一条数据变更（admin 写库时插入 data_change_outbox 的语义）。
type Change struct {
	Table   string          // "model_services" | "endpoints" | ...
	Op      Op              // upsert | delete
	PK      string          // 主键 string 形态
	Payload json.RawMessage // upsert 实体 JSON；delete 可空
	Version int64           // 单调递增；通常 time.Now().UnixNano()
}

// AppendTx 在同一事务里追加 outbox 行。
//
// 用法（admin store 的 Create/Update/Delete）：
//
//	err := db.Transaction(func(tx *gorm.DB) error {
//	    if err := tx.Create(&ms).Error; err != nil { return err }
//	    return cdcoutbox.AppendTx(tx, cdcoutbox.Change{
//	        Table:   "model_services",
//	        Op:      cdcoutbox.OpUpsert,
//	        PK:      strconv.FormatInt(ms.ID, 10),
//	        Payload: must(json.Marshal(ms)),
//	    })
//	})
//
// **关键**：必须用同一个 tx；不能在事务外 INSERT outbox（这样无原子性，
// admin commit 但 outbox 漏写 → 缓存永久 stale）。
type TxExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// AppendTx 在事务（任何实现 ExecContext 的对象）里追加 outbox 行。
//
// gorm.DB 有 Exec；sqlx.Tx 有 ExecContext。Admin 端可用 db.Exec(...) 直传。
func AppendTx(ctx context.Context, tx TxExecer, c Change) error {
	if c.Table == "" || c.PK == "" {
		return fmt.Errorf("outbox: table/pk required (got table=%q pk=%q)", c.Table, c.PK)
	}
	if c.Op == "" {
		c.Op = OpUpsert
	}
	if c.Version == 0 {
		c.Version = time.Now().UnixNano()
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO data_change_outbox (table_name, op, pk, payload, version) VALUES (?, ?, ?, ?, ?)`,
		c.Table, string(c.Op), c.PK, []byte(c.Payload), c.Version,
	)
	return err
}

// =============================================================================
// Relay
// =============================================================================

// RelayConfig 配置 OutboxRelay 后台 worker。
type RelayConfig struct {
	DB             *sqlx.DB
	Redis          *redis.Client
	Channel        string        // Redis PUBSUB channel；默认 llm-gateway.invalidate
	CacheKeyPrefix string        // Redis 实体缓存 key 前缀；默认 llm:cache
	CacheTTL       time.Duration // Redis 缓存 key 过期；默认 1h（防 Redis 数据永驻）
	PollInterval   time.Duration // 默认 200ms
	BatchSize      int           // 默认 100；单次 SELECT 最多取多少行
	Logger         *slog.Logger
}

// OutboxRelay admin 端后台 goroutine：
//
//	for {
//	    rows = SELECT * FROM data_change_outbox WHERE sent_at IS NULL ORDER BY id LIMIT N
//	    for r in rows:
//	        Redis SET cache key (TTL) + PUBLISH invalidation channel
//	        UPDATE outbox SET sent_at=NOW() WHERE id=?
//	    sleep poll_interval
//	}
//
// **At-least-once**：网络抖动 / Redis 失败时 row 不 mark sent，下个 poll 再试。
// 重复发布到 Redis 是幂等的（SET + PUBLISH 都安全）。
type OutboxRelay struct {
	cfg    RelayConfig
	stop   chan struct{}
	wg     sync.WaitGroup
	logger *slog.Logger
}

// NewRelay 构造一个 relay。Run() 之前不发起 IO。
func NewRelay(cfg RelayConfig) *OutboxRelay {
	if cfg.Channel == "" {
		cfg.Channel = "llm-gateway.invalidate"
	}
	if cfg.CacheKeyPrefix == "" {
		cfg.CacheKeyPrefix = "llm:cache"
	}
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 1 * time.Hour
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 200 * time.Millisecond
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &OutboxRelay{cfg: cfg, stop: make(chan struct{}), logger: log}
}

// Run 启动 worker（非阻塞）；Stop() 等其退出。
func (r *OutboxRelay) Run(ctx context.Context) {
	if r.cfg.DB == nil || r.cfg.Redis == nil {
		r.logger.Warn("cdcoutbox.Relay: missing DB or Redis; not starting")
		return
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		t := time.NewTicker(r.cfg.PollInterval)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				r.tick(ctx)
			}
		}
	}()
}

// Stop 优雅停止。
func (r *OutboxRelay) Stop() {
	close(r.stop)
	r.wg.Wait()
}

// CacheKey 给定表 + PK 返回 Redis 缓存 key。
//
// 格式：<prefix>:<table>:<pk>，例如 "llm:cache:model_services:5"
func (r *OutboxRelay) CacheKey(table, pk string) string {
	return r.cfg.CacheKeyPrefix + ":" + table + ":" + pk
}

// InvalidationMessage Redis PUBSUB 通道上的消息体。
type InvalidationMessage struct {
	Table   string `json:"table"`
	Op      string `json:"op"`
	PK      string `json:"pk"`
	Version int64  `json:"version"`
}

// tick 处理一批 outbox 行。
func (r *OutboxRelay) tick(ctx context.Context) {
	rows, err := r.cfg.DB.QueryxContext(ctx,
		`SELECT id, table_name, op, pk, payload, version FROM data_change_outbox
		 WHERE sent_at IS NULL ORDER BY id LIMIT ?`,
		r.cfg.BatchSize,
	)
	if err != nil {
		r.logger.WarnContext(ctx, "cdcoutbox: select unsent", "err", err.Error())
		return
	}
	defer func() { _ = rows.Close() }()

	type row struct {
		ID      int64           `db:"id"`
		Table   string          `db:"table_name"`
		Op      string          `db:"op"`
		PK      string          `db:"pk"`
		Payload []byte          `db:"payload"`
		Version int64           `db:"version"`
	}
	var batch []row
	for rows.Next() {
		var r row
		if err := rows.StructScan(&r); err != nil {
			continue
		}
		batch = append(batch, r)
	}
	if len(batch) == 0 {
		return
	}

	for _, row := range batch {
		key := r.CacheKey(row.Table, row.PK)
		switch row.Op {
		case string(OpUpsert):
			if len(row.Payload) > 0 {
				if err := r.cfg.Redis.Set(ctx, key, row.Payload, r.cfg.CacheTTL).Err(); err != nil {
					r.logger.WarnContext(ctx, "cdcoutbox: redis SET failed", "key", key, "err", err.Error())
					continue // 不 mark sent；下次重试
				}
			}
		case string(OpDelete):
			_ = r.cfg.Redis.Del(ctx, key).Err()
		}
		msg, _ := json.Marshal(InvalidationMessage{
			Table:   row.Table,
			Op:      row.Op,
			PK:      row.PK,
			Version: row.Version,
		})
		if err := r.cfg.Redis.Publish(ctx, r.cfg.Channel, msg).Err(); err != nil {
			r.logger.WarnContext(ctx, "cdcoutbox: redis PUBLISH failed", "err", err.Error())
			continue
		}
		if _, err := r.cfg.DB.ExecContext(ctx,
			`UPDATE data_change_outbox SET sent_at=NOW(6) WHERE id=?`, row.ID); err != nil {
			r.logger.WarnContext(ctx, "cdcoutbox: mark sent failed", "id", row.ID, "err", err.Error())
		}
	}
}
