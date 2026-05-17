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
// 设计目标：source of truth（必须能透传写失败错误）+ 高吞吐（target 10k+ QPS）。
//
// 性能要点：
//
//   - 单 syscall 写入：把 payload + '\n' 合并到一个 buffer 后一次 Write，避免
//     "Write(payload) + Write('\n')" 的双 syscall 开销（用户态↔内核态切换 ~0.5-2μs/次）。
//   - 无显式锁：*os.File.Write 内部用 fdmutex 串行化同 FD 的写——只要每次 Publish
//     是单次 Write 调用，内核保证整行原子落地，不会跟其他 goroutine 的 Publish 交错。
//   - buffer pool：用 sync.Pool 复用 append 用的字节切片，减少 GC 压力。超大 payload
//     的 buffer 不放回池子（按 maxPooledBufSize 截断），避免长尾把池子撑大。
//
// 没用 log/slog 写：slog `Handler.Handle()` 的 error 被 `_ = ...` 吞掉，调用方拿不到
// "写成功了没"——违反 source-of-truth 的可观测性要求。详见 docs/05 §5。
type FileOutbox struct {
	f *os.File
}

// 池化的 buffer，复用 append 缓冲；payload 通常几百 B 到几 KB，预分配 1KiB 起步。
var fileOutboxBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 1024)
		return &b
	},
}

// maxPooledBufSize 超过这个 cap 的 buffer 不放回池子（避免被偶发大消息撑大）。
const maxPooledBufSize = 64 * 1024

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
// 单次 syscall 写入 evt.Payload + '\n'；不做 fsync（v0.1 牺牲耐久性换吞吐——
// 由 OS page cache 异步刷盘；如需更强 durability 由调用方按需 fsync）。
func (o *FileOutbox) Publish(_ context.Context, evt *OutboxEvent) error {
	if evt == nil {
		return errors.New("usage: FileOutbox.Publish: nil event")
	}
	bp := fileOutboxBufPool.Get().(*[]byte)
	buf := (*bp)[:0]
	buf = append(buf, evt.Payload...)
	buf = append(buf, '\n')

	// *os.File.Write 用 fdmutex 串行化；单次 Write 调用内核保证原子落地，
	// 即使其他 goroutine 同时 Publish 也不会出现 JSONL 行交错。
	_, err := o.f.Write(buf)

	// 大 payload 撑出来的 buffer 不放回池子，避免后续小 publish 持续占用大内存
	if cap(buf) <= maxPooledBufSize {
		*bp = buf[:0]
		fileOutboxBufPool.Put(bp)
	}
	return err
}

// Close 关闭底层文件；实现 io.Closer 以便 graceful shutdown 调用。
//
// 关闭操作本身只调用一次有效（Close 后 f 置 nil，重复 Close 返回 nil）；为
// 简化实现这里复用 *os.File 自己的 close-once 语义不再加额外保护，但保持
// 调用幂等。多次 Close 在 Go 里是安全的（第二次返回 ErrClosed），这里我们
// 主动检查 nil 避免暴露 ErrClosed 给 caller。
func (o *FileOutbox) Close() error {
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
