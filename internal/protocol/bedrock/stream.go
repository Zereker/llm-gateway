package bedrock

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/aws/smithy-go/eventstream"
	"github.com/tidwall/gjson"

	"github.com/zereker/llm-gateway/internal/protocol"
)

// DecodeTransport implements protocol.TransportDecoder: both of Bedrock's
// streaming responses (InvokeModelWithResponseStream and ConverseStream) use
// AWS event-stream binary framing (Content-Type vnd.amazon.eventstream), but
// the frame *payload* shape differs between the two APIs -- see
// eventStreamReader (InvokeModel) vs converseEventStreamReader (Converse)'s
// doc comments. DecodeTransport is dispatched by Factory type alone (no
// per-request state — see this package's doc comment), so which one applies
// is sniffed from resp.Request's URL path suffix: Go's http.Client always
// populates Response.Request with the request that produced it.
//
// For non-streaming (JSON) responses this returns nil: no decoding is needed, the
// bytes go straight to the handler.
func (Factory) DecodeTransport(resp *http.Response) io.Reader {
	if !strings.Contains(resp.Header.Get("Content-Type"), "vnd.amazon.eventstream") {
		return nil
	}

	if resp.Request != nil && strings.HasSuffix(resp.Request.URL.Path, "/converse-stream") {
		return &converseEventStreamReader{dec: eventstream.NewDecoder(), src: resp.Body}
	}

	return &eventStreamReader{dec: eventstream.NewDecoder(), src: resp.Body}
}

// eventStreamReader decodes an AWS event-stream frame by frame, restoring the
// Anthropic event inside each frame into a `data: <json>\n\n` SSE line (the format
// the openai_anthropic handler expects).
//
// **Frame payload shape** (Bedrock InvokeModelWithResponseStream):
//
//	{"bytes":"<base64(anthropic event json)>"}
//
// Base64-decoding that yields the native Anthropic stream event
// (message_start / content_block_delta / ...).
type eventStreamReader struct {
	dec     *eventstream.Decoder
	src     io.Reader
	pending []byte // decoded SSE bytes awaiting consumption by Read
	err     error
}

func (r *eventStreamReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil // io.Reader convention: len(p)==0 should not return anything other than (0,nil)
	}

	for len(r.pending) == 0 {
		if r.err != nil {
			return 0, r.err
		}

		msg, err := r.dec.Decode(r.src, nil)
		if err != nil {
			r.err = err
			// Edge case: if decoding returns data alongside an error (e.g. the final
			// frame's payload plus io.EOF), emit the data first and defer the error to
			// the next Read, so we don't drop the last frame.
			if sse := frameToSSE(msg.Payload); sse != nil {
				r.pending = sse
				break
			}

			return 0, err
		}
		// Bedrock delivers mid-stream failures (throttling / modelStreamErrorException /
		// …) as frames with :message-type=exception, which smithy does not surface as a
		// Go error. These must be explicitly detected and converted into an error —
		// otherwise they'd be mistaken for a clean truncation: the client would get no
		// error, FeedErr wouldn't be set, and the truncated stream could be billed/cached
		// as a success (an error-propagation blind spot).
		if exErr := frameException(msg); exErr != nil {
			r.err = exErr
			return 0, exErr
		}

		if sse := frameToSSE(msg.Payload); sse != nil {
			r.pending = sse
		}
		// Empty/irrelevant frames (e.g. metrics) → keep decoding the next frame
	}

	n := copy(p, r.pending)
	r.pending = r.pending[n:]

	return n, nil
}

// frameException checks whether a frame is an AWS event-stream exception/error
// frame (:message-type is exception or error); if so, returns an error carrying the
// exception type + payload so the caller can abort the stream. Normal event frames
// (:message-type=event) return nil.
func frameException(msg eventstream.Message) error {
	mt := msg.Headers.Get(":message-type")
	if mt == nil {
		return nil // no message-type header: treat as a normal frame (frameToSSE handles it)
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

// frameToSSE converts a frame payload into a single line of Anthropic SSE.
func frameToSSE(payload []byte) []byte {
	if len(payload) == 0 {
		return nil
	}

	b64 := gjson.GetBytes(payload, "bytes").String()

	event := payload // fallback: pass through as-is when the frame lacks the bytes wrapper (exception frames)
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

// Compile-time assertion: Factory implements TransportDecoder.
var _ protocol.TransportDecoder = Factory{}
