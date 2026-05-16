package contentlog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// =============================================================================
// FilePublisher：JSONL append；本地排障 / dev
// =============================================================================

// FilePublisher 把 Record 序列化成 JSONL 追加写文件。
//
// 适合本地开发 / 单实例小流量；生产高吞吐用 KafkaPublisher。
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

// =============================================================================
// 占位防 unused import
// =============================================================================
var _ = bytes.NewBuffer
var _ = errors.New
