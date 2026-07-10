package extractor

import "bytes"

// nextSSEFrame splits off the first complete SSE event from buf. An event ends
// at a blank line, which upstreams terminate as either "\n\n" (LF) or
// "\r\n\r\n" (CRLF) — some OpenAI-compatible upstreams (certain vLLM/proxy
// configs) use CRLF. Scanning only for "\n\n" would buffer a CRLF stream
// forever, so the usage frame (and the client's tokens) would never be seen.
// Returns the event bytes, the remaining buffer, and whether a full event was
// found. Lines within the event still carry a trailing \r, which
// extractDataPayload trims.
func nextSSEFrame(buf []byte) (event, rest []byte, ok bool) {
	lf := bytes.Index(buf, []byte("\n\n"))
	crlf := bytes.Index(buf, []byte("\r\n\r\n"))
	switch {
	case lf < 0 && crlf < 0:
		return nil, buf, false
	case crlf < 0 || (lf >= 0 && lf < crlf):
		return buf[:lf], buf[lf+2:], true
	default:
		return buf[:crlf], buf[crlf+4:], true
	}
}

// extractDataPayload takes the payload after "data: " from a single SSE line;
// returns nil for non-data lines.
//
// Compatible with the SSE spec's zero-or-one space after "data:" + trims a
// trailing \r.
func extractDataPayload(line []byte) []byte {
	const prefix = "data:"
	if !bytes.HasPrefix(line, []byte(prefix)) {
		return nil
	}
	rest := line[len(prefix):]
	if len(rest) > 0 && rest[0] == ' ' {
		rest = rest[1:]
	}
	return bytes.TrimSpace(rest)
}
