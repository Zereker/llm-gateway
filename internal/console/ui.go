package console

import _ "embed"

// indexHTML is the control plane's single-file Web UI (Phase 3), embedded
// into the binary at compile time — zero external resources, no build step,
// served same-origin from the same process as the API (no CORS needed).
//
// **Deliberately vanilla / single-file**: this is an ops back office, not a
// consumer-facing product; a self-contained HTML file (inline CSS/JS) is
// enough, and deployment has zero dependencies. If this ever needs to become
// an external SaaS console, invest in proper frontend engineering then.
//
//go:embed ui/index.html
var indexHTML []byte
