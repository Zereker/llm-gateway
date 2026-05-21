package repo

import (
	"database/sql/driver"
	"encoding/json"
)

// RoutingConfig 是 endpoints.routing 列：vendor-specific 的 URL / 端点定位字段。
//
// 字段稀疏：每个 vendor 只填自己用得到的几个：
//
//	openai/anthropic/gemini/ark/deepseek...：URL（完整 chat completions 端点）
//	bedrock：Region（URL 由 region 推导）
//	vertex：Project + Location + Publisher
//	azure-openai：URL（resource 端点）+ Deployment + APIVersion
//
// 不加密——这些都是 URL / region / project ID 等，不算秘密。
type RoutingConfig struct {
	URL        string `json:"url,omitempty"`
	Region     string `json:"region,omitempty"`
	Project    string `json:"project,omitempty"`
	Location   string `json:"location,omitempty"`
	Publisher  string `json:"publisher,omitempty"`
	Deployment string `json:"deployment,omitempty"`
	APIVersion string `json:"api_version,omitempty"`
}

// Scan 实现 sql.Scanner。
func (r *RoutingConfig) Scan(value any) error {
	if value == nil {
		*r = RoutingConfig{}
		return nil
	}
	b, err := bytesFromScan(value, "RoutingConfig")
	if err != nil {
		return err
	}
	if len(b) == 0 {
		*r = RoutingConfig{}
		return nil
	}
	return json.Unmarshal(b, r)
}

// Value 实现 driver.Valuer；零值写 NULL。
//
// 注意：endpoints.routing 列在 schema 标 NOT NULL，所以 deployer 写表时应该
// 至少填 URL（或 region 等）；零 RoutingConfig 写出 NULL 会 INSERT 失败。
// 这是有意的——强迫 deployer 显式给一个 routing。
func (r RoutingConfig) Value() (driver.Value, error) {
	if (r == RoutingConfig{}) {
		return nil, nil
	}
	return json.Marshal(r)
}
