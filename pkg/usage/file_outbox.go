package usage

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// FileOutbox 把 OutboxEvent.Payload 按 JSONL 格式追加到本地文件。
//
// 用于本地开发 / 单机部署。生产高吞吐场景建议换 Kafka 实现。
//
// 实现并发安全：mutex 串行化 Write，避免 JSONL 行交错。
type FileOutbox struct {
	mu sync.Mutex
	f  *os.File
}

// NewFileOutbox 打开（或创建）path 文件，以 append 模式写入。
//
// path 所在目录会自动创建。
func NewFileOutbox(path string) (*FileOutbox, error) {
	if path == "" {
		return nil, errors.New("usage: FileOutbox path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &FileOutbox{f: f}, nil
}

// Publish 实现 OutboxPublisher.Publish。
//
// 写入 evt.Payload + '\n'；不做 fsync（v0.1 牺牲耐久性换吞吐）。
func (o *FileOutbox) Publish(_ context.Context, evt *OutboxEvent) error {
	if evt == nil {
		return errors.New("usage: FileOutbox.Publish: nil event")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, err := o.f.Write(evt.Payload); err != nil {
		return err
	}
	_, err := o.f.Write([]byte{'\n'})
	return err
}

// Close 关闭底层文件；实现 io.Closer 以便 graceful shutdown 调用。
func (o *FileOutbox) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.f == nil {
		return nil
	}
	err := o.f.Close()
	o.f = nil
	return err
}

// 编译期断言：FileOutbox 满足 OutboxPublisher + io.Closer。
var (
	_ OutboxPublisher = (*FileOutbox)(nil)
	_ io.Closer       = (*FileOutbox)(nil)
)
