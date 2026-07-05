package repo

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
)

// AuthConfig is the content of the endpoints.auth column: vendor-tagged auth config.
//
// **The whole column is AES-256-GCM encrypted** (handled transparently by
// Scanner/Valuer). It's not just the secret field inside Payload that's
// encrypted, but the entire JSON blob — so any new field added later is
// automatically protected, with no need to annotate it field by field.
//
// **MarshalJSON masks it**: admin tool GET responses / log dumps show "***"
// for the whole payload. To get the plaintext you must explicitly decode via
// DecodePayload (used by the adapter).
//
// **Type -> Payload schema mapping**:
//
//	"bearer"     → BearerAuth  (openai/deepseek/ark/qwen/moonshot/zhipu...)
//	"x-api-key"  → XAPIKeyAuth (anthropic)
//	"gemini-key" → GeminiAuth  (gemini AI Studio)
//	"aws-sigv4"  → AWSSigV4Auth (bedrock)
//	"oauth2-sa"  → OAuth2SAAuth (vertex AI)
//
// Adding a vendor just needs a new type constant + payload struct + DecodePayload
// in the adapter — zero schema changes.
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
//
// The full SA JSON string is embedded in the payload; AES-GCM encryption is
// done at the AuthConfig column level. Suited to centrally managed service
// account scenarios.
type OAuth2SAAuth struct {
	ServiceAccountJSON string `json:"service_account_json"`
}

// VertexADCAuth: uses Application Default Credentials (the gateway process's environment)
//
// No credentials are stored in the DB at all — discovered automatically from
// the ADC file / GOOGLE_APPLICATION_CREDENTIALS env var / GCE metadata server
// on the machine running the gateway.
//
// Suited to:
//   - dev: testing Vertex locally after `gcloud auth application-default login`
//   - GCE / GKE: workload identity / metadata server injection
//
// Not suited to: multi-account BYOC (each account bringing its own SA JSON) —
// that case uses OAuth2SAAuth instead.
//
// The payload is currently an empty struct (a placeholder; future fields like
// Scopes / QuotaProject overrides could be added).
type VertexADCAuth struct {
	// Optional: override the OAuth scopes; defaults to cloud-platform if unset
	Scopes []string `json:"scopes,omitempty"`
}

// authConfigJSON is the actual JSON shape used internally by Scanner/Valuer
// (without the MarshalJSON masking). Not exposed outside the package.
type authConfigJSON struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// Scan implements sql.Scanner: DB ciphertext -> decrypt -> unmarshal plain JSON.
func (a *AuthConfig) Scan(value any) error {
	if value == nil {
		*a = AuthConfig{}
		return nil
	}
	b, err := bytesFromScan(value, "AuthConfig")
	if err != nil {
		return err
	}
	if len(b) == 0 {
		*a = AuthConfig{}
		return nil
	}
	plain, err := decryptBlob(b)
	if err != nil {
		return fmt.Errorf("AuthConfig: decrypt: %w", err)
	}
	var raw authConfigJSON
	if err := json.Unmarshal(plain, &raw); err != nil {
		return fmt.Errorf("AuthConfig: unmarshal: %w", err)
	}
	*a = AuthConfig{Type: raw.Type, Payload: raw.Payload}
	return nil
}

// Value implements driver.Valuer: marshal plaintext JSON -> encrypt -> store.
//
// Note: can't just json.Marshal(a) directly, since a.MarshalJSON masks the
// payload; must go through authConfigJSON to pass it through untouched.
func (a AuthConfig) Value() (driver.Value, error) {
	if a.Type == "" {
		return nil, nil
	}
	plain, err := json.Marshal(authConfigJSON{Type: a.Type, Payload: a.Payload})
	if err != nil {
		return nil, fmt.Errorf("AuthConfig: marshal: %w", err)
	}
	enc, err := encryptBlob(plain)
	if err != nil {
		return nil, fmt.Errorf("AuthConfig: encrypt: %w", err)
	}
	// The MySQL JSON column accepts a string; this is really ciphertext, but a
	// valid JSON-shaped string (we use base64, so it won't break JSON parsing).
	return string(enc), nil
}

// MarshalJSON masks the payload; neither admin tool GET responses nor log
// dumps leak it.
//
// If the caller genuinely needs the plaintext, they must use DecodePayload.
func (a AuthConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type    string `json:"type"`
		Payload string `json:"payload"`
	}{Type: a.Type, Payload: "***"})
}

// DecodePayload unmarshals a.Payload into T.
//
// Usage (adapter):
//
//	bearer, err := repo.DecodePayload[repo.BearerAuth](ep.Auth)
//	if err != nil { ... }
//	req.Header.Set("Authorization", "Bearer "+bearer.APIKey)
//
// The caller is responsible for choosing the right T — typically by checking
// ep.Auth.Type first.
func DecodePayload[T any](a AuthConfig) (T, error) {
	var t T
	if len(a.Payload) == 0 {
		return t, errors.New("AuthConfig: empty payload")
	}
	if err := json.Unmarshal(a.Payload, &t); err != nil {
		return t, fmt.Errorf("AuthConfig: decode %T: %w", t, err)
	}
	return t, nil
}

// EncodePayload is a helper: serializes a typed payload into RawMessage when
// the deployer builds an AuthConfig.
func EncodePayload(authType string, payload any) (AuthConfig, error) {
	if authType == "" {
		return AuthConfig{}, errors.New("AuthConfig: empty type")
	}
	if payload == nil {
		return AuthConfig{Type: authType}, nil
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return AuthConfig{}, fmt.Errorf("AuthConfig: encode %T: %w", payload, err)
	}
	return AuthConfig{Type: authType, Payload: b}, nil
}
