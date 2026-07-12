package bedrock

import (
	"io"

	"github.com/aws/smithy-go/eventstream"
)

// converseEventStreamReader decodes ConverseStream's AWS event-stream frame
// by frame into `event: <type>\ndata: <json>\n\n` lines — the shape
// internal/translator/openai_bedrock's response handler expects (see that
// package's doc comment for why: unlike InvokeModel's frames, which wrap an
// Anthropic-native SSE event with its own "type" field inside {"bytes":
// base64(...)}, Converse's frame payload IS the raw event JSON with no
// "type" field of its own — the type lives only in the frame's
// `:event-type` header, so it has to be threaded through some other way for
// the response handler to see it, and `event: <type>` mirrors the exact
// two-line convention Anthropic's own real SSE already uses).
type converseEventStreamReader struct {
	dec     *eventstream.Decoder
	src     io.Reader
	pending []byte
	err     error
}

func (r *converseEventStreamReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.pending) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		msg, err := r.dec.Decode(r.src, nil)
		if err != nil {
			r.err = err
			if sse := converseFrameToSSE(msg); sse != nil {
				r.pending = sse
				break
			}
			return 0, err
		}
		// Same exception-frame handling as InvokeModel's reader (see
		// frameException's doc comment in stream.go) — this is a plain
		// function, not a method, so it's shared as-is.
		if exErr := frameException(msg); exErr != nil {
			r.err = exErr
			return 0, exErr
		}
		if sse := converseFrameToSSE(msg); sse != nil {
			r.pending = sse
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// converseFrameToSSE converts one Converse event frame into
// "event: <type>\ndata: <payload>\n\n". Frames with no `:event-type` header
// (and no payload) carry nothing translatable and are skipped.
func converseFrameToSSE(msg eventstream.Message) []byte {
	if len(msg.Payload) == 0 {
		return nil
	}
	et := msg.Headers.Get(":event-type")
	if et == nil {
		return nil
	}
	out := make([]byte, 0, len(msg.Payload)+len(et.String())+16)
	out = append(out, "event: "...)
	out = append(out, et.String()...)
	out = append(out, '\n')
	out = append(out, "data: "...)
	out = append(out, msg.Payload...)
	out = append(out, '\n', '\n')
	return out
}
