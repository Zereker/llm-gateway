package domain

// RequestEnvelope is the product of the M3 Envelope middleware.
//
// Business logic makes decisions on RawBytes (the raw bytes) + Model (the
// model field M3 extracted from the body) + SourceProtocol / Modality; body
// translation / field mapping is entirely delegated to the individual
// translator implementations under internal/translator, this struct carries **no**
// canonicalization responsibility.
//
// Design philosophy: M3 only does "read body + grab model for routing", it
// does not do parameter parsing; a CanonicalRequest — a "unified internal
// representation" — used to exist but had no consumer for any of its fields,
// so it was removed (v1.0 review decision). Upstream / client protocol shape
// conversion is handled individually by internal/translator/<src>_<tgt>/.
//
// **No RequestTime here**: latency calculation just uses rc.StartTime
// (written by M1); there's no independent consumer of the M3 entry moment,
// adding one would just create a second source of truth.
type RequestEnvelope struct {
	// RawBytes is the raw bytes of the client request body; the body has
	// already been reset via NopCloser on c.Request.Body, so downstream
	// reads of c.Request.Body get the same content.
	RawBytes []byte

	// Model is the model name extracted from the body's top-level `model`
	// field (M5 ModelService looks up the catalog from this). All three
	// client protocols (OpenAI Chat / Anthropic Messages / OpenAI Responses)
	// have a top-level model field, so this field can always be filled in.
	Model string

	// SourceProtocol is the client protocol (written by the M1 routing-side
	// WithSourceProtocol middleware).
	SourceProtocol Protocol

	// Modality is the modality (written by the M1 routing-side
	// WithSourceProtocol middleware).
	Modality Modality
}
