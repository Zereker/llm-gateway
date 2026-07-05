package domain

import (
	"encoding/json"
	"time"
)

// Endpoint is the business model of an upstream access point (docs/06 §3).
//
// Exists in parallel with repo.Endpoint: repo.Endpoint carries db: tags +
// Scanner/Valuer to handle the DB; domain.Endpoint is the pure struct used by
// middleware/schedule/adapter (no SQL dependency). repo converts between them
// at the SQL boundary via ToDomainEndpoint().
type Endpoint struct {
	ID      int64
	Name    string
	Vendor  string
	Model   string
	Group   string
	Weight  uint32
	Enabled bool

	// Protocol is the protocol this endpoint's upstream speaks (OpenAI /
	// Anthropic / Gemini / Responses).
	//
	// **Required** — the zero value ProtoUnknown makes DefaultLookup.Get
	// return nil directly, and the eligibility filter drops the endpoint.
	// The deployer must configure this explicitly when writing an endpoint.
	//
	// **Design rationale**: protocol is an endpoint-level property, not a
	// vendor-level one — the same vendor can very well have multiple
	// endpoints on different protocols (e.g. when Anthropic offers both the
	// native Messages API and an OpenAI-compatible API, the two endpoints
	// are Protocol=Anthropic / Protocol=OpenAI respectively).
	//
	// **Protocol-conversion decision**: the dispatcher compares
	// (env.SourceProtocol, ep.Protocol):
	//   - equal → identity translator passes through (no actual conversion)
	//   - unequal → translator.Find(src, ep.Protocol) looks up a cross-protocol translator; missing one drops eligibility
	Protocol Protocol

	Auth         AuthConfig
	Routing      RoutingConfig
	Quota        QuotaConfig
	Capabilities EndpointCapabilities

	// Quirks is the endpoint-level body/header tweak DSL (pkg/protocol/quirks).
	// Empty = no-op. combine.go runs body and header adjustments together
	// after the translator and before adapter BuildRequest, then hands the
	// final body + headers to the adapter for assembly in one pass.
	Quirks json.RawMessage

	Extra json.RawMessage

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// EndpointCapabilities marks an endpoint's capabilities (modality +
// self-hosted sub-fields).
//
// **JSON shape** (stored in the endpoints.capabilities column; written by
// deployer SQL):
//
//	{
//	  "modalities":         ["chat", "embedding"],
//	  "self_hosted":        false,
//	  "prefix_cache_enabled": true
//	}
type EndpointCapabilities struct {
	// Modalities is the whitelist of modalities this endpoint can accept
	// (a subset of the vendor's capabilities).
	//
	// **Semantics**: narrowing only, never widening — eligibility requires
	// that both the endpoint's list AND the vendor Factory's declared
	// SupportedModalities include the current request's modality. When
	// empty, it falls back to the vendor's ceiling. This way, if a deployer
	// mistakenly configures `["tts"]` on a chat-only vendor, the request
	// still can't sneak into the selector (see pkg/dispatch/eligibility.go).
	//
	// **Typical usage**: the OpenAI vendor declares chat / embedding / image
	// / audio all at once, but a deployer who only bought gpt-4o chat quota
	// for one endpoint locks it down with `["chat"]`, so that endpoint
	// doesn't accidentally pick up requests like /v1/embeddings.
	Modalities []Modality `json:"modalities,omitempty"`

	SelfHosted          bool   `json:"self_hosted,omitempty"`
	KVMetricEndpoint    string `json:"kv_metric_endpoint,omitempty"`
	HealthProbeEndpoint string `json:"health_probe_endpoint,omitempty"`
	PrefixCacheEnabled  bool   `json:"prefix_cache_enabled,omitempty"`
}

// EndpointForm is derived from Capabilities.
type EndpointForm int

const (
	FormVendor EndpointForm = iota
	FormSelfHosted
)

func (f EndpointForm) String() string {
	if f == FormSelfHosted {
		return "self_hosted"
	}
	return "vendor"
}

// Form is the derivation method.
func (e *Endpoint) Form() EndpointForm {
	if e.Capabilities.SelfHosted {
		return FormSelfHosted
	}
	return FormVendor
}

// =============================================================================
// AuthConfig / Auth payload types (vendor auth — docs/06 §3)
// =============================================================================

// AuthConfig is the vendor-tagged auth configuration.
//
// Note: domain.AuthConfig does **not** come with a Scanner/Valuer (production
// DB encryption is done at the repo layer). Business code uniformly obtains
// the typed payload via DecodePayload[T].
type AuthConfig struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// Known auth type constants (payload structs below).
const (
	AuthTypeBearer    = "bearer"
	AuthTypeXAPIKey   = "x-api-key"
	AuthTypeGeminiKey = "gemini-key"
	AuthTypeAWSSigV4  = "aws-sigv4"
	AuthTypeOAuth2SA  = "oauth2-sa"
	AuthTypeVertexADC = "vertex-adc"
)

// BearerAuth: Authorization: Bearer <api_key>
type BearerAuth struct {
	APIKey string `json:"api_key"`
}

// XAPIKeyAuth: x-api-key: <api_key> header (Anthropic)
type XAPIKeyAuth struct {
	APIKey string `json:"api_key"`
}

// GeminiAuth: ?key=<api_key> or x-goog-api-key header (Gemini AI Studio)
type GeminiAuth struct {
	APIKey string `json:"api_key"`
}

// AWSSigV4Auth: AWS Signature Version 4 (Bedrock)
type AWSSigV4Auth struct {
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	Region    string `json:"region"`
}

// OAuth2SAAuth: Service Account JSON exchanged for an OAuth2 access token (Vertex AI)
type OAuth2SAAuth struct {
	ServiceAccountJSON string `json:"service_account_json"`
}

// VertexADCAuth: uses Application Default Credentials
type VertexADCAuth struct {
	Scopes []string `json:"scopes,omitempty"`
}

// DecodePayload deserializes AuthConfig.Payload into T.
//
//	bearer, err := domain.DecodePayload[domain.BearerAuth](ep.Auth)
//	if err != nil { ... }
//	req.Header.Set("Authorization", "Bearer "+bearer.APIKey)
func DecodePayload[T any](a AuthConfig) (T, error) {
	var t T
	if len(a.Payload) == 0 {
		return t, errEmptyAuthPayload
	}
	if err := json.Unmarshal(a.Payload, &t); err != nil {
		return t, err
	}
	return t, nil
}

// EncodePayload is a helper that serializes a typed payload into RawMessage
// when constructing an AuthConfig.
func EncodePayload(authType string, payload any) (AuthConfig, error) {
	if authType == "" {
		return AuthConfig{}, errEmptyAuthType
	}
	if payload == nil {
		return AuthConfig{Type: authType}, nil
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return AuthConfig{}, err
	}
	return AuthConfig{Type: authType, Payload: b}, nil
}

// =============================================================================
// RoutingConfig / QuotaConfig (pure business-side structs)
// =============================================================================

// RoutingConfig holds vendor-specific URL / endpoint-locating fields (docs/02 §1 vendor routing).
type RoutingConfig struct {
	URL        string `json:"url,omitempty"`
	Region     string `json:"region,omitempty"`
	Project    string `json:"project,omitempty"`
	Location   string `json:"location,omitempty"`
	Publisher  string `json:"publisher,omitempty"`
	Deployment string `json:"deployment,omitempty"`
	APIVersion string `json:"api_version,omitempty"`
}

// QuotaConfig is an endpoint's hard upstream rate-limit constraints (sparse
// fields: nil = unlimited).
type QuotaConfig struct {
	RPM                *uint32 `json:"rpm,omitempty"`
	TPM                *uint32 `json:"tpm,omitempty"`
	RPS                *uint32 `json:"rps,omitempty"`
	ConcurrentRequests *uint32 `json:"concurrent_requests,omitempty"`
}

// IsEmpty reports whether no quota field is set at all.
func (q QuotaConfig) IsEmpty() bool {
	return q.RPM == nil && q.TPM == nil && q.RPS == nil && q.ConcurrentRequests == nil
}

// sentinel errors (avoid import "errors" in domain core types file)
type authErr string

func (e authErr) Error() string { return string(e) }

const (
	errEmptyAuthPayload authErr = "AuthConfig: empty payload"
	errEmptyAuthType    authErr = "AuthConfig: empty type"
)
