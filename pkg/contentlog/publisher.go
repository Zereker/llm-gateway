package contentlog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
)

// =============================================================================
// FilePublisher：JSONL append
// =============================================================================

// FilePublisher 把 Record 序列化成 JSONL 追加写文件。
//
// 这是 Content Log 唯一支持的真实后端：gateway 只写本地 JSONL，由 fluent-bit /
// vector 投递到下游各 sink（归档 / 检索 / 内容安全后审 / 训练数据回流）。详见
// docs/architecture/05-metering-billing.md §2 + docs/07-configuration.md §2。
//
// 文件轮转 / 压缩 / 清理由外部 logrotate 或日志收集器负责，不在本进程内做。
type FilePublisher struct {
	mu sync.Mutex
	w  io.WriteCloser
}

// NewFilePublisher 打开（或创建）指定路径文件用于 append 写。
func NewFilePublisher(path string) (*FilePublisher, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("contentlog: open file: %w", err)
	}
	return &FilePublisher{w: f}, nil
}

// Publish 序列化 + append 一行 JSON + 换行。
func (p *FilePublisher) Publish(_ context.Context, r *Record) error {
	buf, err := json.Marshal(r)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, err := p.w.Write(buf); err != nil {
		return err
	}
	_, err = p.w.Write([]byte("\n"))
	return err
}

// Close 关闭文件。
func (p *FilePublisher) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.w == nil {
		return nil
	}
	err := p.w.Close()
	p.w = nil
	return err
}

// =============================================================================
// NoopPublisher：默认禁用；不输出任何 Record
// =============================================================================

// NoopPublisher 永远成功且不做任何事；driver=none 时使用。
type NoopPublisher struct{}

func (NoopPublisher) Publish(_ context.Context, _ *Record) error { return nil }
