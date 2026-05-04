// Package server 是进程 lifecycle manager：把"打开 infra 依赖 + 服务运行 +
// 收到信号优雅退出 + 倒序 close 全部依赖"这套样板代码集中到一处。
//
// cmd/gateway 与 cmd/admin 都通过本包装配，省掉每个 cmd 各自写一份生命周期代码。
//
// 用法：
//
//	s := server.New(slog.Default())
//	sqldb,    _ := s.OpenDB(cfg.Database)             // 自动注册 db.Close
//	producer, _ := s.NewKafkaProducer(cfg.Kafka)      // 自动注册 producer.Close
//	s.AddCloser("file-outbox", outbox.Close)          // 自定义 cleanup
//	engine := router.NewEngine(...)
//	return s.Serve(addr, engine, readTimeout, shutdownTimeout)
//
// 三件事自动发生：
//
//  1. SIGINT / SIGTERM 触发 graceful shutdown
//  2. http.Server.Shutdown 等正在处理的请求
//  3. 倒序（LIFO）跑全部注册的 closer——后建依赖先关，避免悬空引用
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/redis/go-redis/v9"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/zereker-labs/ai-gateway/pkg/infra"
)

// Server 持有 closer 链 + 日志器；不强制单例，每个 binary 各自 New 一个。
type Server struct {
	mu      sync.Mutex
	closers []closeEntry
	log     *slog.Logger
}

type closeEntry struct {
	name string
	fn   func() error
}

// New 构造一个空的 Server；log 为 nil 时回落到 slog.Default()。
func New(log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{log: log}
}

// AddCloser 注册一个 cleanup 回调。
//
// 关闭顺序是 LIFO（后注册的先 Close），与依赖建立顺序相反——这样上层依赖
// （比如持有 *sqlx.DB 的 repo）不会在底层依赖（*sqlx.DB 本身）已 Close 后还被使用。
func (s *Server) AddCloser(name string, fn func() error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closers = append(s.closers, closeEntry{name: name, fn: fn})
}

// OpenDB 通过 infra.Open 构造 *sqlx.DB 并自动注册其 Close。
func (s *Server) OpenDB(cfg infra.DBConfig) (*sqlx.DB, error) {
	db, err := infra.Open(cfg)
	if err != nil {
		return nil, err
	}
	s.AddCloser("db", db.Close)
	return db, nil
}

// NewKafkaProducer 通过 infra.NewKafkaProducer 构造 producer 并自动注册其 Close。
//
// 同一个 producer 可以喂给多个 outbox（usage / audit / ...）；
// 在 cmd 装配方持有 producer 引用做共享时，**只**通过本方法 open 一次，
// 不要把 outbox.Close 也 AddCloser（会双关）。
func (s *Server) NewKafkaProducer(cfg infra.KafkaConfig) (*infra.KafkaProducer, error) {
	p, err := infra.NewKafkaProducer(cfg)
	if err != nil {
		return nil, err
	}
	s.AddCloser("kafka", p.Close)
	return p, nil
}

// OpenRedis 通过 infra.OpenRedis 构造 *redis.Client 并自动注册其 Close。
//
// gateway 启动期 ping fail-fast；M6 RateLimit 必须有 Redis（不再有内存兜底）。
func (s *Server) OpenRedis(cfg infra.RedisConfig) (*redis.Client, error) {
	rdb, err := infra.OpenRedis(cfg)
	if err != nil {
		return nil, err
	}
	s.AddCloser("redis", rdb.Close)
	return rdb, nil
}

// Close 倒序跑所有 registered closer。Idempotent（连续调用安全）。
//
// 测试 / 非 Serve 路径（比如 cmd 内部 buildEngine 的 error fast-fail）用这个。
// Serve 在退出前也会调用，多调一次不会重复 close 同一资源（内部 closer 列表清空）。
func (s *Server) Close() {
	s.mu.Lock()
	closers := s.closers
	s.closers = nil
	s.mu.Unlock()

	for i := len(closers) - 1; i >= 0; i-- {
		e := closers[i]
		if err := e.fn(); err != nil {
			s.log.Warn("close failed", "name", e.name, "err", err)
		}
	}
}

// Serve 启动 http.Server 并阻塞到收到 SIGINT/SIGTERM 或服务自身错误，
// 然后做 Shutdown(graceful) + Close（倒序跑 closer 链）。
//
// 返回值：服务运行期错误（监听失败、accept 失败等）；shutdown 错误只 log 不 return。
func (s *Server) Serve(addr string, handler http.Handler, readTimeout, shutdownTimeout time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return s.serveCtx(ctx, addr, handler, readTimeout, shutdownTimeout)
}

// serveCtx 是 Serve 的内部实现：ctx 取消触发 shutdown，方便测试用 cancel 模拟信号。
//
// **HTTP/2 (h2c) 支持**：用 h2c.NewHandler 包一层让客户端可以走明文 HTTP/2
// （PRI 升级或 prior knowledge）。HTTP/1.1 客户端不受影响——h2c handler
// 自动按请求 ALPN/preface 区分协议。生产 ingress 一般做 TLS 终结后走 h2c
// 到上游网关，本配置直接支持。
func (s *Server) serveCtx(ctx context.Context, addr string, handler http.Handler, readTimeout, shutdownTimeout time.Duration) error {
	h2s := &http2.Server{}
	srv := &http.Server{
		Addr:              addr,
		Handler:           h2c.NewHandler(handler, h2s),
		ReadHeaderTimeout: readTimeout,
	}

	serverErr := make(chan error, 1)
	go func() {
		s.log.Info("server listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	var serveErr error
	select {
	case err := <-serverErr:
		serveErr = err
	case <-ctx.Done():
		s.log.Info("shutdown signal received")
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		s.log.Warn("http shutdown", "err", err)
	}
	s.Close()
	return serveErr
}
