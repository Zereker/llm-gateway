// Command seed-e2e: a one-shot seed tool for e2e smoke tests.
//
// Run infra.Migrate before starting the gateway; this tool writes the
// minimal business data to MySQL needed for a curl smoke test: 1 account /
// 1 quota_policy / 1 model_service / 1 subscription / 1 endpoint (pointing
// at mockupstream:9090, bearer auth) / 1 api_key.
//
// Usage:
//
//	go run ./scripts/seed-e2e \
//	  -dsn "root:@tcp(localhost:3306)/llm_gateway?parseTime=true&charset=utf8mb4" \
//	  -data-key "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" \
//	  -upstream "http://localhost:9090/v1/chat/completions" \
//	  -api-key "sk-test-alice"
//
// Output: the plaintext api key the client should use as Bearer, printed
// directly to stdout (for the smoke script to curl with).
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

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/infra"
	"github.com/zereker/llm-gateway/internal/repo"
)

func main() {
	dsn := flag.String("dsn", "", "MySQL DSN")
	dataKey := flag.String("data-key", "", "endpoints.auth KEK (hex 32 bytes)")
	upstream := flag.String("upstream", "http://localhost:9090/v1/chat/completions", "upstream URL")
	apiKey := flag.String("api-key", "sk-test-alice", "plaintext api key (written into the Bearer header)")
	model := flag.String("model", "gpt-4o", "model name (written into the client request body)")
	flag.Parse()

	if *dsn == "" || *dataKey == "" {
		log.Fatal("seed-e2e: -dsn and -data-key required")
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
	if err := seed(ctx, db, *upstream, *apiKey, *model); err != nil {
		log.Fatalf("seed: %v", err)
	}

	fmt.Fprintln(os.Stdout, *apiKey)
}

func seed(ctx context.Context, db *sqlx.DB, upstreamURL, apiKey, model string) error {
	// 1) quota_policy
	res, err := db.ExecContext(ctx, `
		INSERT INTO quota_policies (name, description, rule_json)
		VALUES ('e2e-smoke', 'e2e smoke test policy',
		        JSON_OBJECT('default', JSON_OBJECT('rpm', 600, 'tpm', 1000000)))
		ON DUPLICATE KEY UPDATE name=name`)
	if err != nil {
		return fmt.Errorf("quota_policies: %w", err)
	}
	policyID, _ := res.LastInsertId()
	if policyID == 0 {
		// ON DUPLICATE KEY meant nothing was inserted; look it up.
		if err := db.GetContext(ctx, &policyID, `SELECT id FROM quota_policies WHERE name='e2e-smoke'`); err != nil {
			return fmt.Errorf("re-fetch policy id: %w", err)
		}
	}

	// 2) account
	if _, err := db.ExecContext(ctx, `
		INSERT INTO accounts (pin, name, quota_policy_id)
		VALUES ('e2e-acme', 'E2E ACME', ?)
		ON DUPLICATE KEY UPDATE name=name`, policyID); err != nil {
		return fmt.Errorf("accounts: %w", err)
	}

	// 3) model_service
	res, err = db.ExecContext(ctx, `
		INSERT INTO model_services (service_id, model)
		VALUES (?, ?)
		ON DUPLICATE KEY UPDATE service_id=service_id`,
		"e2e/"+model, model)
	if err != nil {
		return fmt.Errorf("model_services: %w", err)
	}
	msID, _ := res.LastInsertId()
	if msID == 0 {
		if err := db.GetContext(ctx, &msID, `SELECT id FROM model_services WHERE model=?`, model); err != nil {
			return fmt.Errorf("re-fetch model_service: %w", err)
		}
	}

	// 4) account_model_subscriptions
	if _, err := db.ExecContext(ctx, `
		INSERT INTO account_model_subscriptions (account_id, model_service_id)
		VALUES ('e2e-acme', ?)
		ON DUPLICATE KEY UPDATE account_id=account_id`, msID); err != nil {
		return fmt.Errorf("subscriptions: %w", err)
	}

	// 5) endpoint
	auth, err := repo.EncodePayload(domain.AuthTypeBearer, domain.BearerAuth{APIKey: "sk-upstream-dontcare"})
	if err != nil {
		return fmt.Errorf("encode auth: %w", err)
	}
	routing := repo.RoutingConfig{URL: upstreamURL}
	ep := &repo.Endpoint{
		Name:     "e2e-mockupstream",
		Vendor:   "openai",
		Protocol: "openai",
		Model:    model,
		Group:    "default",
		Weight:   100,
		Enabled:  true,
		Auth:     auth,
		Routing:  routing,
	}
	if _, err := db.NamedExecContext(ctx, `
		INSERT INTO endpoints
		  (name, vendor, protocol, model, group_name, weight, enabled,
		   auth, routing, quota, capabilities, quirks, extra)
		VALUES
		  (:name, :vendor, :protocol, :model, :group_name, :weight, :enabled,
		   :auth, :routing, :quota, :capabilities, :quirks, :extra)
		ON DUPLICATE KEY UPDATE name=name`, ep); err != nil {
		return fmt.Errorf("endpoints: %w", err)
	}

	// 6) api_key — store the sha256 hex hash of the plaintext (e.g. sk-test-alice)
	hash := repo.HashAPIKey(apiKey)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO api_keys
		  (account_id, api_key_hash, api_key_prefix, api_key_id,
		   sub_account_id, group_name, quota_policy_id, enabled)
		VALUES
		  ('e2e-acme', ?, ?, 'ak_e2e_alice', 'alice@e2e', 'default', ?, 1)
		ON DUPLICATE KEY UPDATE account_id=account_id`,
		hash, apiKeyPrefix(apiKey), policyID); err != nil {
		// MySQL's ErrDup is distinct from sql.ErrNoRows; checking SQLState is
		// overkill here, so just ignore duplicates.
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
	// MySQL duplicate entry: error 1062; do a rough string match here to
	// avoid pulling in a mysql-specific error type.
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
