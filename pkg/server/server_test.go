package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zereker/llm-gateway/pkg/infra"
)

func TestServer_AddCloserOrderIsLIFO(t *testing.T) {
	s := New(silent())
	var order []string
	s.AddCloser("a", func() error { order = append(order, "a"); return nil })
	s.AddCloser("b", func() error { order = append(order, "b"); return nil })
	s.AddCloser("c", func() error { order = append(order, "c"); return nil })

	s.Close()

	want := []string{"c", "b", "a"}
	if len(order) != 3 {
		t.Fatalf("ran %d closers, want 3", len(order))
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, order[i], want[i])
		}
	}
}

func TestServer_CloseChainContinuesOnError(t *testing.T) {
	s := New(silent())
	var ran []string
	s.AddCloser("a", func() error { ran = append(ran, "a"); return nil })
	s.AddCloser("b", func() error { ran = append(ran, "b"); return errors.New("boom") })
	s.AddCloser("c", func() error { ran = append(ran, "c"); return nil })

	s.Close()

	if len(ran) != 3 {
		t.Errorf("ran = %v, want all 3 (error in middle should not stop chain)", ran)
	}
}

func TestServer_CloseIsIdempotent(t *testing.T) {
	s := New(silent())
	calls := 0
	s.AddCloser("x", func() error { calls++; return nil })

	s.Close()
	s.Close() // 第二次不应再跑（closer 列表已清空）
	s.Close()

	if calls != 1 {
		t.Errorf("calls = %d, want 1 (Close must be idempotent)", calls)
	}
}

func TestServer_OpenDBRegistersClose(t *testing.T) {
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		t.Skip("MYSQL_DSN not set; skipping OpenDB integration test")
	}
	s := New(silent())
	db, err := s.OpenDB(infra.DBConfig{Driver: infra.DriverMySQL, DSN: dsn})
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if db == nil {
		t.Fatal("db is nil")
	}
	if err := db.Ping(); err != nil {
		t.Errorf("Ping before Close: %v", err)
	}

	s.Close()

	if err := db.Ping(); err == nil {
		t.Error("Ping after Close should fail (db should be closed)")
	}
}

func TestServer_NewKafkaProducerRegistersClose(t *testing.T) {
	s := New(silent())
	p, err := s.NewKafkaProducer(infra.KafkaConfig{Brokers: []string{"localhost:9092"}})
	if err != nil {
		t.Fatalf("NewKafkaProducer: %v", err)
	}
	if p == nil {
		t.Fatal("producer is nil")
	}

	// Close 链应该跑到 kafka 的 Close（never-used producer 关掉是 no-op，不报错）
	s.Close()
}

func TestServer_NewKafkaProducer_BadConfig(t *testing.T) {
	s := New(silent())
	_, err := s.NewKafkaProducer(infra.KafkaConfig{}) // empty brokers
	if err == nil {
		t.Fatal("want error for empty brokers")
	}
	// 关键：失败时不能错误注册 closer
	if len(s.closers) != 0 {
		t.Errorf("closers = %d, want 0 after failed open", len(s.closers))
	}
}

func TestServer_ServeShutdownOnCtxCancel(t *testing.T) {
	s := New(silent())

	var closeRan atomic.Bool
	s.AddCloser("test", func() error { closeRan.Store(true); return nil })

	// 先占一个空闲端口（避免 OS 复用导致 listen 冲突）
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- s.serveCtx(ctx, addr, handler, time.Second, 2*time.Second)
	}()

	// 等监听起来再发请求验证（最多 1s）
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel() // 触发 shutdown

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serveCtx returned err: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveCtx didn't return after ctx cancel")
	}

	if !closeRan.Load() {
		t.Error("close chain didn't run on shutdown")
	}
}

func TestServer_ServeReturnsListenError(t *testing.T) {
	s := New(silent())
	// "0.0.0.0:1" 之类 privileged port 在大部分环境会拒绝，但更可靠的做法是
	// 用一个明确无效的 addr：":99999"（端口越界）
	err := s.serveCtx(context.Background(), ":99999",
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}),
		time.Second, time.Second)
	if err == nil {
		t.Error("want listen error for invalid port")
	}
}

// silent 返回一个丢弃所有日志的 logger，让测试输出干净。
func silent() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
