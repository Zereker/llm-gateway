package dispatch

import (
	"strconv"
	"strings"
)

// HeaderAttemptCap defines the semantics of the attempts ceiling:
//
//	cfg.Default = cfg.Selector.MaxAttempts (default 3)
//	the client's X-Gateway-Max-Attempts header may only override it in the
//	**tighter** (smaller) direction; it can never exceed Default (to prevent
//	maliciously raising the gateway's attempts ceiling).
//
// **Header parsing**: middleware/schedule.go reads
// c.GetHeader("X-Gateway-Max-Attempts") before calling Dispatch and stuffs
// the raw string into Input.AttemptCapOverride; this Policy parses + clamps
// it.
type HeaderAttemptCap struct {
	Default int // global default; must be > 0
}

// Resolve computes the attempt cap for this request.
//
//	override > 0 && override < Default → override
//	otherwise → Default
func (h HeaderAttemptCap) Resolve(in Input) int {
	def := h.Default
	if def <= 0 {
		def = 3
	}

	raw := strings.TrimSpace(in.AttemptCapOverride)
	if raw == "" {
		return def
	}

	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}

	if n < def {
		return n
	}

	return def
}
