// Command seed-multivendor seeds one endpoint + one real API key **per
// upstream vendor** (openai / anthropic / gemini / cohere) against a single
// mockupstream instance, so a freshly started local stack (make stack +
// make run-mockupstream + make run-gateway) has, in one step, the same
// multi-vendor business data shape internal/app/gateway's
// TestE2E_MultiVendor_AllProtocols already exercises in-process — letting a
// human (or scripts/e2e-smoke.sh) drive a real compiled gateway binary
// through every client-facing protocol with one seed run.
//
// Idempotent: every insert is ON DUPLICATE KEY UPDATE, so re-running this on
// every stack startup is safe and just leaves the same rows in place.
//
// Usage:
//
//	go run ./scripts/seed-multivendor \
//	  -dsn "root:@tcp(localhost:3306)/llm_gateway?parseTime=true&charset=utf8mb4" \
//	  -data-key "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" \
//	  -mock-base "http://127.0.0.1:9090"
//
// Output: one "<vendor> model=<model> api_key=<key>" line per vendor,
// printed to stdout so a caller can curl each one without hardcoding values.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"

	"github.com/zereker/llm-gateway/internal/infra"
	"github.com/zereker/llm-gateway/internal/repo"
)

// scenario mirrors internal/app/gateway/fieldmatrix_multivendor_test.go's
// vendorScenario: adding a fifth vendor here is one more literal in
// scenarios(), nothing else changes.
type scenario struct {
	vendor      string
	protocol    string
	model       string
	authType    string
	authPayload any // repo.BearerAuth / XAPIKeyAuth / GeminiAuth / ... — whatever authType requires
	apiKey      string
	upstreamURL string // full URL on the mockupstream instance this vendor's route lives at
}

func scenarios(mockBase string) []scenario {
	return []scenario{
		{
			vendor: "openai", protocol: "openai", model: "mock-openai-model",
			authType: repo.AuthTypeBearer, authPayload: repo.BearerAuth{APIKey: "sk-upstream-mock-openai"},
			apiKey:      "sk-mv-openai",
			upstreamURL: mockBase + "/v1/chat/completions",
		},
		{
			vendor: "anthropic", protocol: "anthropic", model: "mock-anthropic-model",
			authType: repo.AuthTypeXAPIKey, authPayload: repo.XAPIKeyAuth{APIKey: "sk-upstream-mock-anthropic"},
			apiKey:      "sk-mv-anthropic",
			upstreamURL: mockBase + "/v1/messages",
		},
		{
			vendor: "gemini", protocol: "gemini", model: "mock-gemini-model",
			authType: repo.AuthTypeGeminiKey, authPayload: repo.GeminiAuth{APIKey: "sk-upstream-mock-gemini"},
			apiKey: "sk-mv-gemini",
			// gemini's session.BuildRequest forwards Routing.URL verbatim (no
			// {model} templating) -- the segment here only has to look like a
			// real generateContent URL to mockupstream's handleGemini, which
			// only parses it for the echoed modelVersion field.
			upstreamURL: mockBase + "/v1beta/models/mock-gemini-model:generateContent",
		},
		{
			vendor: "cohere", protocol: "cohere", model: "mock-cohere-model",
			authType: repo.AuthTypeBearer, authPayload: repo.BearerAuth{APIKey: "sk-upstream-mock-cohere"},
			apiKey:      "sk-mv-cohere",
			upstreamURL: mockBase + "/v2/chat",
		},
	}
}

func main() {
	dsn := flag.String("dsn", "", "MySQL DSN")
	dataKey := flag.String("data-key", "", "endpoints.auth KEK (hex 32 bytes)")
	mockBase := flag.String("mock-base", "http://127.0.0.1:9090", "mockupstream base URL (see cmd/mockupstream's route table)")
	flag.Parse()

	if *dsn == "" || *dataKey == "" {
		log.Fatal("seed-multivendor: -dsn and -data-key required")
	}
	if err := repo.SetDataKey(*dataKey); err != nil {
		log.Fatalf("set data key: %v", err)
	}

	db, err := sqlx.Connect("mysql", *dsn)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := infra.Migrate(ctx, db); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO accounts (pin, name)
		VALUES ('e2e-multivendor', 'E2E Multivendor')
		ON DUPLICATE KEY UPDATE name=name`); err != nil {
		log.Fatalf("accounts: %v", err)
	}

	for _, sc := range scenarios(*mockBase) {
		if err := seedOne(ctx, db, sc); err != nil {
			log.Fatalf("seed %s: %v", sc.vendor, err)
		}
		fmt.Fprintf(os.Stdout, "%s model=%s api_key=%s\n", sc.vendor, sc.model, sc.apiKey)
	}
}

func seedOne(ctx context.Context, db *sqlx.DB, sc scenario) error {
	res, err := db.ExecContext(ctx, `
		INSERT INTO model_services (service_id, model)
		VALUES (?, ?)
		ON DUPLICATE KEY UPDATE service_id=service_id`,
		sc.vendor+"/"+sc.model, sc.model)
	if err != nil {
		return fmt.Errorf("model_services: %w", err)
	}
	msID, _ := res.LastInsertId()
	if msID == 0 {
		if err := db.GetContext(ctx, &msID, `SELECT id FROM model_services WHERE model=?`, sc.model); err != nil {
			return fmt.Errorf("re-fetch model_service: %w", err)
		}
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO account_model_subscriptions (account_id, model_service_id)
		VALUES ('e2e-multivendor', ?)
		ON DUPLICATE KEY UPDATE account_id=account_id`, msID); err != nil {
		return fmt.Errorf("subscriptions: %w", err)
	}

	auth, err := repo.EncodePayload(sc.authType, sc.authPayload)
	if err != nil {
		return fmt.Errorf("encode auth: %w", err)
	}
	ep := &repo.Endpoint{
		Name:     "mv_" + sc.vendor,
		Vendor:   sc.vendor,
		Protocol: sc.protocol,
		Model:    sc.model,
		Group:    "default",
		Weight:   100,
		Enabled:  true,
		Auth:     auth,
		Routing:  repo.RoutingConfig{URL: sc.upstreamURL},
	}
	if _, err := db.NamedExecContext(ctx, `
		INSERT INTO endpoints
		  (name, vendor, protocol, model, group_name, weight, enabled,
		   auth, routing, quota, capabilities, quirks, extra)
		VALUES
		  (:name, :vendor, :protocol, :model, :group_name, :weight, :enabled,
		   :auth, :routing, :quota, :capabilities, :quirks, :extra)
		ON DUPLICATE KEY UPDATE
		  protocol=VALUES(protocol), model=VALUES(model), enabled=VALUES(enabled),
		  auth=VALUES(auth), routing=VALUES(routing)`, ep); err != nil {
		return fmt.Errorf("endpoints: %w", err)
	}

	hash := repo.HashAPIKey(sc.apiKey)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO api_keys
		  (account_id, api_key_hash, api_key_prefix, api_key_id,
		   sub_account_id, group_name, enabled)
		VALUES
		  ('e2e-multivendor', ?, ?, ?, ?, 'default', 1)
		ON DUPLICATE KEY UPDATE account_id=account_id`,
		hash, apiKeyPrefix(sc.apiKey), "ak_mv_"+sc.vendor, sc.vendor+"@e2e-multivendor"); err != nil {
		if !isDuplicateErr(err) {
			return fmt.Errorf("api_keys: %w", err)
		}
	}

	return nil
}

func apiKeyPrefix(plain string) string {
	if len(plain) > 7 {
		return plain[:7]
	}
	return plain
}

func isDuplicateErr(err error) bool {
	if err == nil || err == sql.ErrNoRows {
		return false
	}
	msg := err.Error()
	return contains(msg, "Duplicate entry") || contains(msg, "Error 1062")
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
