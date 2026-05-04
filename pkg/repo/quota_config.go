package repo

import (
	"database/sql/driver"
	"encoding/json"
)

// QuotaConfig 是 endpoints.quota 列：上游 API 给的限流硬约束。
//
// **稀疏字段语义**：
//   - nil = 此 vendor 不存在该 quota（schema 上 GeminiAuth 只有 RPM，TPM/RPS 都 nil）
//   - 0   = 该 quota 存在但"无限制"（罕见，一般都是非零正数）
//   - 正数 = 配额本体
//
// M6 RateLimit middleware（v0.5+ 完整实现）按字段存在性决定要不要拦：
//   - 存在 → 加入限流计数器
//   - 不存在 → 不拦
//
// 不加密——quota 数字非敏感。
type QuotaConfig struct {
	RPM                *uint32 `json:"rpm,omitempty"`
	TPM                *uint32 `json:"tpm,omitempty"`
	RPS                *uint32 `json:"rps,omitempty"`
	ConcurrentRequests *uint32 `json:"concurrent_requests,omitempty"`
}

// Scan 实现 sql.Scanner。
func (q *QuotaConfig) Scan(value any) error {
	if value == nil {
		*q = QuotaConfig{}
		return nil
	}
	b, err := bytesFromScan(value, "QuotaConfig")
	if err != nil {
		return err
	}
	if len(b) == 0 {
		*q = QuotaConfig{}
		return nil
	}
	return json.Unmarshal(b, q)
}

// Value 实现 driver.Valuer；全空写 NULL（schema 是 nullable JSON 列）。
func (q QuotaConfig) Value() (driver.Value, error) {
	if q.RPM == nil && q.TPM == nil && q.RPS == nil && q.ConcurrentRequests == nil {
		return nil, nil
	}
	return json.Marshal(q)
}

// IsEmpty 判断 QuotaConfig 是否一个 quota 字段都没填。
func (q QuotaConfig) IsEmpty() bool {
	return q.RPM == nil && q.TPM == nil && q.RPS == nil && q.ConcurrentRequests == nil
}
