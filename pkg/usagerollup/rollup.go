// Package usagerollup 把 usage outbox（JSONL 文件，source of truth）**增量**聚合进
// usage_daily 表，喂控制面 dashboard。
//
// **定位**：派生视图，不是计费真源（计费仍以 outbox 事件为准）。之所以从**文件**而
// 非 Kafka 消费——file 是 outbox 的 source of truth（docs/05 §5），不依赖 broker
// 可用性；Kafka 消费者是后续可选优化。
//
// **增量**：sidecar offset 文件记住已处理到的字节位置，每次只读新增的完整行，
// ON DUPLICATE KEY 累加进 usage_daily。只推进到最后一个换行符——半行（正在写入的
// 尾行）留到下次。因此反复 Run 不会重复计数。
package usagerollup

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/jmoiron/sqlx"

	"github.com/zereker/llm-gateway/pkg/usage"
)

// Rollup 增量聚合器。
type Rollup struct {
	db         *sqlx.DB
	filePath   string
	offsetPath string
}

// New 构造 Rollup；offset sidecar 固定为 <filePath>.rollup-offset。
func New(db *sqlx.DB, filePath string) *Rollup {
	return &Rollup{db: db, filePath: filePath, offsetPath: filePath + ".rollup-offset"}
}

// Result 一次 Run 的统计。
type Result struct {
	Events    int   // 成功聚合的事件数
	Skipped   int   // 解析失败 / 缺 account_id|model 的行
	BytesRead int64 // 本次消费的字节
	NewOffset int64 // 推进后的 offset
}

// aggKey 聚合维度。
type aggKey struct {
	account string
	model   string
	day     string // YYYY-MM-DD (UTC)
}

type aggVal struct {
	input, output, total, requests int64
}

// Run 读文件从上次 offset 起的新完整行，聚合并累加进 usage_daily，推进 offset。
//
// 文件不存在视为"还没有用量"，返回空 Result（不报错）。
func (r *Rollup) Run(ctx context.Context) (Result, error) {
	offset, err := r.readOffset()
	if err != nil {
		return Result{}, err
	}

	f, err := os.Open(r.filePath)
	if errors.Is(err, os.ErrNotExist) {
		return Result{NewOffset: offset}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("usagerollup: open %s: %w", r.filePath, err)
	}
	defer f.Close()

	// 文件被 logrotate 截短（新文件比 offset 短）时，从头开始——否则 seek 越界。
	if fi, statErr := f.Stat(); statErr == nil && fi.Size() < offset {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return Result{}, fmt.Errorf("usagerollup: seek: %w", err)
	}

	agg := make(map[aggKey]*aggVal)
	res := Result{}
	reader := bufio.NewReader(f)
	for {
		line, rerr := reader.ReadString('\n')
		if rerr == io.EOF {
			// 尾部半行（无换行符）——不消费，留到下次。
			break
		}
		if rerr != nil {
			return Result{}, fmt.Errorf("usagerollup: read: %w", rerr)
		}
		res.BytesRead += int64(len(line))

		var ev usage.UsageEvent
		if json.Unmarshal([]byte(line), &ev) != nil {
			res.Skipped++
			continue
		}
		acct := ev.Usage.Meta.AccountID
		model := ev.Usage.Meta.Model
		if acct == "" || model == "" {
			res.Skipped++
			continue
		}
		k := aggKey{account: acct, model: model, day: ev.CreatedAt.UTC().Format("2006-01-02")}
		v := agg[k]
		if v == nil {
			v = &aggVal{}
			agg[k] = v
		}
		v.input += ev.Usage.Input
		v.output += ev.Usage.Output
		v.total += ev.Usage.Total
		v.requests++
		res.Events++
	}

	if len(agg) > 0 {
		if err := r.upsert(ctx, agg); err != nil {
			return Result{}, err
		}
	}

	res.NewOffset = offset + res.BytesRead
	if err := r.writeOffset(res.NewOffset); err != nil {
		return Result{}, err
	}
	return res, nil
}

// upsert 在一个事务里 ON DUPLICATE KEY 累加每个 (account, model, day) 聚合。
func (r *Rollup) upsert(ctx context.Context, agg map[aggKey]*aggVal) error {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("usagerollup: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const q = `INSERT INTO usage_daily
		(account_id, model, day, input_tokens, output_tokens, total_tokens, requests)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			input_tokens  = input_tokens  + VALUES(input_tokens),
			output_tokens = output_tokens + VALUES(output_tokens),
			total_tokens  = total_tokens  + VALUES(total_tokens),
			requests      = requests      + VALUES(requests)`
	for k, v := range agg {
		if _, err := tx.ExecContext(ctx, q,
			k.account, k.model, k.day, v.input, v.output, v.total, v.requests); err != nil {
			return fmt.Errorf("usagerollup: upsert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("usagerollup: commit: %w", err)
	}
	return nil
}

func (r *Rollup) readOffset() (int64, error) {
	b, err := os.ReadFile(r.offsetPath)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("usagerollup: read offset: %w", err)
	}
	n, err := strconv.ParseInt(string(bytesTrimSpace(b)), 10, 64)
	if err != nil {
		return 0, nil // offset 文件损坏 → 从头（会 ON DUPLICATE 累加，故用前必须清表；生产极少）
	}
	return n, nil
}

// writeOffset 原子写（temp + rename），避免崩溃留半个 offset。
func (r *Rollup) writeOffset(n int64) error {
	tmp := r.offsetPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(n, 10)), 0o644); err != nil {
		return fmt.Errorf("usagerollup: write offset: %w", err)
	}
	if err := os.Rename(tmp, r.offsetPath); err != nil {
		return fmt.Errorf("usagerollup: rename offset: %w", err)
	}
	return nil
}

func bytesTrimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && (b[start] == ' ' || b[start] == '\n' || b[start] == '\r' || b[start] == '\t') {
		start++
	}
	for end > start && (b[end-1] == ' ' || b[end-1] == '\n' || b[end-1] == '\r' || b[end-1] == '\t') {
		end--
	}
	return b[start:end]
}
