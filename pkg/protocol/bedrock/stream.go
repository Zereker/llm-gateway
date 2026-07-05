package bedrock

import (
	"encoding/base64"
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
	for len(r.pending) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		msg, err := r.dec.Decode(r.src, nil)
		if err != nil {
			r.err = err
			return 0, err
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
