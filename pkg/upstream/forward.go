package upstream

import (
	"context"
	"io"
	"net/http"
	"sync"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/translator"
)

// chunkBufPool 复用 stream forward 用的 4KiB read buffer。
//
// 高 QPS 流式场景下每请求 make([]byte, 4096) 会显著增加 GC 压力。
// pool 存 *[]byte，避免 sync.Pool interface{} 装箱（Go FAQ 推荐做法）。
//
// 4KiB：典型 SSE chunk 几百字节到 1KiB；4KiB 一次 Read 通常拿一两个 chunk，刚好。
// 走 io.CopyBuffer 时这个 buf 直接喂给 stdlib，零额外分配。
var chunkBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 4096)
		return &b
	},
}

// ForwardResult Forward 完成后的返回值。
//
// Usage 可能为 nil（translator 没解析到 / 上游没返回）。
// FeedErr 是流式过程中的中止错误（resp.Body Read / handler.Feed / Flush 出错）；
// 非 nil 表示流已经从客户端角度看也中断（已写出的字节无法召回）。
type ForwardResult struct {
	Usage   *domain.Usage
	FeedErr error
}

// Forward 把成功上游响应流式 forward 给 ResponseWriter。
//
// **w 是 stdlib `http.ResponseWriter`**：gin.ResponseWriter / echo.Response /
// httptest.NewRecorder 全都自动满足，pkg/upstream 不绑定任何 framework。
//
// 内部用 io.CopyBuffer 驱动 chunk 流动，写入 dst 是 translatorWriter——
// 把 handler.Feed 包装成 io.Writer 形态：每个 chunk 经 Feed 翻译 → 客户端 Write + Flush。
// CopyBuffer 用 chunkBufPool 拿来的 buf 直接做 read buffer，零额外分配。
//
// **不能直接 io.Copy(w, resp.Body)**：每个 chunk 必须经 translator（协议翻译 /
// moderator output 检查 / SSE 重新分帧 / usage 解析），还要在 EOF 后 handler.Flush()
// 收尾——这两件事 io.Copy 都不知道。
//
// **Hook fan-out**（详见 hooks.go）：
//   - UpstreamChunkObserver：translatorWriter.Write 入口、Feed 之前——上游原始 chunk
//   - ClientChunkObserver：inner.Write 之后 + Flush 收尾的 finalOut——客户端实际看到的字节
//
// chunk 切片仅在回调期间有效；observer 要持久化必须自己 copy。
//
// **失败语义**：流式开始（Header 已写）后任何错误都不能回滚状态码。中止错误
// 写在 ForwardResult.FeedErr，caller 自行写 rc.Error / log；客户端看到的是
// 截断的流。
func (s *Sender) Forward(
	ctx context.Context,
	w http.ResponseWriter,
	ep *domain.Endpoint,
	resp *http.Response,
	handler translator.ResponseHandler,
) ForwardResult {
	defer func() { _ = resp.Body.Close() }()

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	flush(w)

	bufPtr := chunkBufPool.Get().(*[]byte)
	defer chunkBufPool.Put(bufPtr)

	tw := &translatorWriter{
		inner:   w,
		handler: handler,
		ctx:     ctx,
		ep:      ep,
		hooks:   s.hooks,
	}
	_, feedErr := io.CopyBuffer(tw, resp.Body, *bufPtr)

	finalOut, usage, fErr := handler.Flush()
	if len(finalOut) > 0 {
		_, _ = w.Write(finalOut)
		flush(w)
		// 最后一截也 fan-out（buffer-then-translate 模式下 Feed 期间不输出，
		// Flush 时一次性翻译完整 body——observer 必须能看到这部分字节）
		s.hooks.fireClientChunk(ctx, ep, finalOut)
	}

	if feedErr == nil && fErr != nil {
		feedErr = fErr
	}

	return ForwardResult{Usage: usage, FeedErr: feedErr}
}

// translatorWriter 把上游 chunk 喂给 translator.ResponseHandler.Feed，把 Feed
// 输出 forward 给真正的 ResponseWriter + flush；同时在 Feed 两侧 fan-out hook。
//
// 这层包装让 io.CopyBuffer 能驱动整个流式管道（src=resp.Body, dst=translatorWriter）。
//
// **Write 返回值约定**：返回 len(chunk)（原 chunk 长度，不是 Feed 输出长度）——
// 这是 io.Copy 协议要求的"消费完整个输入"。Feed 出错时返 (0, err) 让 CopyBuffer 立即停。
type translatorWriter struct {
	inner   http.ResponseWriter
	handler translator.ResponseHandler
	ctx     context.Context
	ep      *domain.Endpoint
	hooks   hookSet
}

func (tw *translatorWriter) Write(chunk []byte) (int, error) {
	// 上游原始 chunk fan-out（Feed 之前）；buffer-then-translate 模式下也只有这里
	// 能拿到上游真实分包字节。
	tw.hooks.fireUpstreamChunk(tw.ctx, tw.ep, chunk)

	out, err := tw.handler.Feed(chunk)
	if err != nil {
		return 0, err
	}
	if len(out) > 0 {
		if _, werr := tw.inner.Write(out); werr != nil {
			return 0, werr
		}
		flush(tw.inner)
		// 客户端 chunk fan-out（inner.Write 之后）；moderator 装饰器拦下的
		// chunk 不会到这里（Feed 早早返 err）。
		tw.hooks.fireClientChunk(tw.ctx, tw.ep, out)
	}
	return len(chunk), nil
}

// flush 调 http.Flusher.Flush；w 没实现 Flusher 时 noop。
//
// 生产环境的 http.ResponseWriter（net/http stdlib server / gin / echo）都实现
// Flusher；只有 httptest.NewRecorder 等测试桩可能不带。退化语义：等 buffer 满
// / EOF 才送，正确性不变只是失去流式。
func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// copyHeaders 把上游响应头拷贝到客户端响应头（除 Content-Length，下游 server 重算）。
func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if k == "Content-Length" {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
