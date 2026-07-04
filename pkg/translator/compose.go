package translator

import (
	"fmt"

	"github.com/zereker/llm-gateway/pkg/domain"
)

// Compose 把两个 translator 串成一个：front (src→pivot) + back (pivot→tgt)。
//
// **用途**：缺对回退（docs/02 §6a）。直连 translator 是高保真首选；registry 里
// 没有 (src, tgt) 直连对时，DefaultLookup 会尝试用 OpenAI 协议做 pivot 组合出
// 一个可用（但可能有损）的翻译链，避免协议对 O(N×M) 增长都要手写。
//
//	请求向：src body → front.TranslateRequest → pivot body → back.TranslateRequest → tgt body
//	响应向：tgt chunks → back.handler（tgt→pivot）→ pivot body → front.handler（pivot→src）→ src body
//
// **有损警告**：双跳会丢失 pivot 协议表达不了的字段（thinking blocks /
// cache_control / vendor 特有参数等）。组合对只作兜底；流量大的组合应尽快
// 补写直连 translator（直连注册后 Find 命中，组合自动退位）。
//
// **前置条件**：front.Target() == back.Source()，否则 panic（组合只发生在
// FindVia 内部，两边都来自 registry，错配即代码 bug，启动期暴露）。
func Compose(front, back Translator) Translator {
	if front == nil || back == nil {
		panic("translator.Compose: nil translator")
	}
	if front.Target() != back.Source() {
		panic(fmt.Sprintf("translator.Compose: pivot mismatch %s != %s",
			front.Target(), back.Source()))
	}
	return &composed{front: front, back: back}
}

// FindVia 先找 (src, tgt) 直连 translator；miss 时尝试经 pivot 组合：
// Find(src, pivot) + Find(pivot, tgt)。任一腿缺失返回 nil。
//
// **直连优先**：手写的高保真对永远压过组合——将来给热门组合补直连实现时
// 不需要改任何调用方。
//
// src == tgt（identity 对）或 src/tgt 就是 pivot 时不会产生冗余组合：
// 直连 Find 已覆盖 identity；单腿等于缺失的直连对，组合同样失败返回 nil。
func FindVia(src, tgt, pivot domain.Protocol) Translator {
	if t := Find(src, tgt); t != nil {
		return t
	}
	front := Find(src, pivot)
	back := Find(pivot, tgt)
	if front == nil || back == nil {
		return nil
	}
	return Compose(front, back)
}

// IsComposed 判断 translator 是否是 pivot 组合产物（供调用方打 lossy warn / metric）。
func IsComposed(t Translator) bool {
	_, ok := t.(*composed)
	return ok
}

// composed 是 Compose 的实现。
type composed struct {
	front Translator // src → pivot
	back  Translator // pivot → tgt
}

func (c *composed) Source() domain.Protocol { return c.front.Source() }
func (c *composed) Target() domain.Protocol { return c.back.Target() }

// TranslateRequest 两跳：src → pivot → tgt。
func (c *composed) TranslateRequest(srcBody []byte) ([]byte, error) {
	pivotBody, err := c.front.TranslateRequest(srcBody)
	if err != nil {
		return nil, fmt.Errorf("compose front (%s→%s): %w", c.front.Source(), c.front.Target(), err)
	}
	tgtBody, err := c.back.TranslateRequest(pivotBody)
	if err != nil {
		return nil, fmt.Errorf("compose back (%s→%s): %w", c.back.Source(), c.back.Target(), err)
	}
	return tgtBody, nil
}

// NewResponseHandler 每请求组合一对 handler。
func (c *composed) NewResponseHandler() ResponseHandler {
	return &composedHandler{
		upstream: c.back.NewResponseHandler(),  // tgt 响应 → pivot 格式
		client:   c.front.NewResponseHandler(), // pivot 格式 → src 格式
	}
}

// composedHandler 串接两级 ResponseHandler。
//
// 上游 chunk 先进 upstream 级（tgt→pivot）；它吐出的 pivot 字节再进 client 级
// （pivot→src）。跨协议 handler 都是 buffer-then-translate 模式（Feed 返 nil、
// Flush 一次性出全量），所以链路上通常只在 Flush 时流动一次；identity 级
// （流式透传）混进链里也成立——Feed 有产出就立刻透给下一级。
type composedHandler struct {
	upstream ResponseHandler // tgt → pivot
	client   ResponseHandler // pivot → src
}

func (h *composedHandler) Feed(chunk []byte) ([]byte, error) {
	mid, err := h.upstream.Feed(chunk)
	if err != nil {
		return nil, err
	}
	if len(mid) == 0 {
		return nil, nil
	}
	return h.client.Feed(mid)
}

// Flush 依次排空两级；usage 优先取 upstream 级（它解析的是真实上游响应；
// client 级看到的是二手 pivot 字节，可能已丢字段）。
func (h *composedHandler) Flush() ([]byte, *domain.Usage, error) {
	midBytes, upUsage, err := h.upstream.Flush()
	if err != nil {
		return nil, nil, err
	}
	var out []byte
	if len(midBytes) > 0 {
		fed, err := h.client.Feed(midBytes)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, fed...)
	}
	tail, clientUsage, err := h.client.Flush()
	if err != nil {
		return nil, nil, err
	}
	out = append(out, tail...)

	usage := upUsage
	if usage == nil {
		usage = clientUsage
	}
	return out, usage, nil
}
