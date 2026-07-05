package bedrock

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/aws/smithy-go/eventstream"
	"github.com/tidwall/gjson"

	"github.com/zereker/llm-gateway/pkg/protocol"
)

// DecodeTransport 实现 protocol.TransportDecoder：Bedrock 流式响应是 AWS event-stream
// 二进制分帧（Content-Type vnd.amazon.eventstream），本函数把它解成 **Anthropic SSE**
// 字节流,交给 openai_anthropic 的 ResponseStream 做 shape 翻译——传输层(解帧)跟协议层
// (Anthropic→OpenAI)干净分离,复用现成的 Anthropic 流式翻译。
//
// 非流式(JSON)响应返 nil：无需解帧,字节直接进 handler。
func (Factory) DecodeTransport(resp *http.Response) io.Reader {
	if !strings.Contains(resp.Header.Get("Content-Type"), "vnd.amazon.eventstream") {
		return nil
	}
	return &eventStreamReader{dec: eventstream.NewDecoder(), src: resp.Body}
}

// eventStreamReader 逐帧解码 AWS event-stream,把每帧内的 Anthropic 事件还原成
// `data: <json>\n\n` 的 SSE 行(openai_anthropic handler 认这个格式)。
//
// **帧 payload 形态**（Bedrock InvokeModelWithResponseStream）：
//
//	{"bytes":"<base64(anthropic event json)>"}
//
// base64 解出来就是 Anthropic 原生流事件(message_start / content_block_delta / ...)。
type eventStreamReader struct {
	dec     *eventstream.Decoder
	src     io.Reader
	pending []byte // 已解出、待 Read 消费的 SSE 字节
	err     error
}

func (r *eventStreamReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil // io.Reader 约定：len(p)==0 不应返回 (0,nil) 以外的东西
	}
	for len(r.pending) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		msg, err := r.dec.Decode(r.src, nil)
		if err != nil {
			r.err = err
			// 极端情形：若解码在给出数据的同时返回错误（如末帧 payload + io.EOF），
			// 先把数据转出去，err 留到下次 Read 再抛，避免丢最后一帧。
			if sse := frameToSSE(msg.Payload); sse != nil {
				r.pending = sse
				break
			}
			return 0, err
		}
		// Bedrock 把 mid-stream 故障（throttling / modelStreamErrorException / …）
		// 作为 :message-type=exception 的帧下发，smithy 不会当成 Go error 返回。
		// 必须显式识别并转成 error，否则会被当成干净截断——客户端收不到错误、
		// FeedErr 不置位、截断流可能被计费/缓存成成功（error-propagation 盲区）。
		if exErr := frameException(msg); exErr != nil {
			r.err = exErr
			return 0, exErr
		}
		if sse := frameToSSE(msg.Payload); sse != nil {
			r.pending = sse
		}
		// 空/无关帧（如 metrics）→ 继续解下一帧
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// frameException 检查一帧是否是 AWS event-stream 的异常/错误帧（:message-type
// 为 exception 或 error）；是则返回携带异常类型 + payload 的 error 供上层中止流。
// 普通事件帧（:message-type=event）返 nil。
func frameException(msg eventstream.Message) error {
	mt := msg.Headers.Get(":message-type")
	if mt == nil {
		return nil // 无 message-type 头：当普通帧处理（frameToSSE 兜底）
	}
	switch mt.String() {
	case "exception", "error":
		name := "unknown"
		if et := msg.Headers.Get(":exception-type"); et != nil {
			name = et.String()
		} else if ec := msg.Headers.Get(":error-code"); ec != nil {
			name = ec.String()
		}
		return fmt.Errorf("bedrock: stream %s %s: %s", mt.String(), name, strings.TrimSpace(string(msg.Payload)))
	default:
		return nil
	}
}

// frameToSSE 把一帧 payload 转成一行 Anthropic SSE。
func frameToSSE(payload []byte) []byte {
	if len(payload) == 0 {
		return nil
	}
	b64 := gjson.GetBytes(payload, "bytes").String()
	event := payload // 兜底：帧不含 bytes 包装(异常帧)时原样透
	if b64 != "" {
		if raw, err := base64.StdEncoding.DecodeString(b64); err == nil {
			event = raw
		}
	}
	out := make([]byte, 0, len(event)+8)
	out = append(out, "data: "...)
	out = append(out, event...)
	out = append(out, '\n', '\n')
	return out
}

// 编译期断言：Factory 实现 TransportDecoder。
var _ protocol.TransportDecoder = Factory{}
