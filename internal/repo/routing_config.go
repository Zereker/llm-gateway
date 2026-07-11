package repo

import (
	"database/sql/driver"
	"encoding/json"
)

// RoutingConfig is the endpoints.routing column: vendor-specific URL / endpoint location fields.
//
// Fields are sparse: each vendor fills in only the few it needs:
//
//	openai/anthropic/gemini/ark/deepseek...: URL (the full chat completions endpoint)
//	bedrock: Region (URL is derived from the region)
//	vertex: Project + Location + Publisher
//	azure-openai: URL (the resource endpoint) + Deployment + APIVersion
//
// Not encrypted — these are all URLs / regions / project IDs etc., not secrets.
type RoutingConfig struct {
	URL        string `json:"url,omitempty"`
	Region     string `json:"region,omitempty"`
	Project    string `json:"project,omitempty"`
	Location   string `json:"location,omitempty"`
	Publisher  string `json:"publisher,omitempty"`
	Deployment string `json:"deployment,omitempty"`
	APIVersion string `json:"api_version,omitempty"`
}

// Scan implements sql.Scanner.
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

// Value implements driver.Valuer; the zero value writes NULL.
//
// Note: the endpoints.routing column is marked NOT NULL in the schema, so the
// deployer's writes should fill in at least URL (or region etc.); writing a
// zero RoutingConfig produces NULL and the INSERT will fail. This is
// intentional — it forces the deployer to explicitly provide a routing.
func (r RoutingConfig) Value() (driver.Value, error) {
	if (r == RoutingConfig{}) {
		return nil, nil
	}
	return json.Marshal(r)
}
