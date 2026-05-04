package repo

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
)

// AuthConfig 是 endpoints.auth 列的内容：vendor-tagged 鉴权配置。
//
// **整列 AES-256-GCM 加密**（Scanner/Valuer 透明处理）。
// 加密的不仅是 Payload 内的 secret 字段，而是整段 JSON——这样未来加新字段
// 也自动受保护，无需逐字段标注。
//
// **MarshalJSON 屏蔽**：admin GET 响应 / 日志 dump 时整段 payload 显示 "***"。
// 想拿到明文必须经 DecodePayload 显式解码（adapter 用）。
//
// **Type → Payload schema 对应表**：
//
//	"bearer"     → BearerAuth  (openai/deepseek/ark/qwen/moonshot/zhipu...)
//	"x-api-key"  → XAPIKeyAuth (anthropic)
//	"gemini-key" → GeminiAuth  (gemini AI Studio)
//	"aws-sigv4"  → AWSSigV4Auth (bedrock)
//	"oauth2-sa"  → OAuth2SAAuth (vertex AI)
//
// 加 vendor 时新增 type 常量 + payload struct + 在 adapter 里 DecodePayload 即可，
// schema 零改动。
type AuthConfig struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// 已知 auth type 常量（payload struct 见下方）。
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

// XAPIKeyAuth: x-api-key: <api_key> 头（Anthropic）
type XAPIKeyAuth struct {
	APIKey string `json:"api_key"`
}

// GeminiAuth: ?key=<api_key> 或 x-goog-api-key 头（Gemini AI Studio）
type GeminiAuth struct {
	APIKey string `json:"api_key"`
}

// AWSSigV4Auth: AWS Signature Version 4（Bedrock）
type AWSSigV4Auth struct {
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	Region    string `json:"region"`
}

// OAuth2SAAuth: Service Account JSON 走 OAuth2 access token（Vertex AI）
//
// 整 SA JSON 字符串嵌入 payload；AES-GCM 加密在 AuthConfig 列层做。
// 适合 admin 中心管理的服务账号场景。
type OAuth2SAAuth struct {
	ServiceAccountJSON string `json:"service_account_json"`
}

// VertexADCAuth: 走 Application Default Credentials（gateway 进程环境）
//
// 不在 DB 存任何凭证——gateway 启动机器上的 ADC 文件 / GOOGLE_APPLICATION_CREDENTIALS
// env / GCE metadata server 自动发现。
//
// 适合：
//   - dev 用 `gcloud auth application-default login` 后本地测试 Vertex
//   - GCE / GKE 上 workload identity / metadata server 注入
//
// 不适合：多租户 BYOC（每个租户带自己的 SA JSON）—— 那种走 OAuth2SAAuth。
//
// payload 当前为空 struct（占位，未来可加 Scopes / QuotaProject 等覆写字段）。
type VertexADCAuth struct {
	// 可选：覆写 OAuth scopes；不填走默认 cloud-platform
	Scopes []string `json:"scopes,omitempty"`
}

// authConfigJSON 用于 Scanner/Valuer 内部的真实 JSON 形态（无 MarshalJSON 屏蔽）。
// 不暴露到包外。
type authConfigJSON struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// Scan 实现 sql.Scanner：DB ciphertext → decrypt → unmarshal plain JSON。
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

// Value 实现 driver.Valuer：marshal plaintext JSON → encrypt → store。
//
// 注意不能直接 json.Marshal(a)，因为 a.MarshalJSON 屏蔽 payload；
// 必须经 authConfigJSON 透传。
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
	// MySQL JSON 列接受 string；本质是密文 + 长度合法的 JSON-shaped string
	// （我们用 base64，不会破坏 JSON 解析）
	return string(enc), nil
}

// MarshalJSON 屏蔽 payload；admin GET / 日志 dump 都不泄漏。
//
// 若上层确实想拿明文，必须用 DecodePayload。
func (a AuthConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type    string `json:"type"`
		Payload string `json:"payload"`
	}{Type: a.Type, Payload: "***"})
}

// DecodePayload 把 a.Payload 反序列化到 T。
//
// 用法（adapter）：
//
//	bearer, err := repo.DecodePayload[repo.BearerAuth](ep.Auth)
//	if err != nil { ... }
//	req.Header.Set("Authorization", "Bearer "+bearer.APIKey)
//
// 调用方负责调对的 T —— 通常先看 ep.Auth.Type 再选 T。
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

// EncodePayload helper：admin 构造 AuthConfig 时把 typed payload 序列化成 RawMessage。
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
