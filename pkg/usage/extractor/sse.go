package extractor

import "bytes"

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
