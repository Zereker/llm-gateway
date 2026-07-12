// Command seed-multivendor seeds one endpoint + one real API key **per
// upstream vendor** against a single mockupstream instance, driven entirely
// by the manifests under testdata/fieldmatrix/endpoints/ (repo root) — see
// internal/cassette/vendorfixture's doc comment for the file shape. A
// freshly started local stack (make stack + make run-mockupstream + make
// run-gateway) gets, in one step, the same multi-vendor business data shape
// internal/app/gateway's TestE2E_MultiVendor_AllProtocols exercises
// in-process — letting a human (or scripts/e2e-smoke-multivendor.sh) drive a
// real compiled gateway binary through every client-facing protocol with one
// seed run. Adding a vendor here is a new JSON file in that directory, not a
// code change.
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

	"github.com/zereker/llm-gateway/internal/cassette"
	"github.com/zereker/llm-gateway/internal/cassette/vendorfixture"
	"github.com/zereker/llm-gateway/internal/infra"
	"github.com/zereker/llm-gateway/internal/repo"
)

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

	scenarios, err := vendorfixture.LoadDir(cassette.TestdataPath("fieldmatrix", "endpoints"))
	if err != nil {
		log.Fatalf("load vendor manifests: %v", err)
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

	for _, sc := range scenarios {
		if err := seedOne(ctx, db, sc, *mockBase); err != nil {
			log.Fatalf("seed %s: %v", sc.Vendor, err)
		}
		fmt.Fprintf(os.Stdout, "%s model=%s api_key=%s\n", sc.Vendor, sc.Model, sc.ClientAPIKey)
	}
}

func seedOne(ctx context.Context, db *sqlx.DB, sc vendorfixture.Scenario, mockBase string) error {
	res, err := db.ExecContext(ctx, `
		INSERT INTO model_services (service_id, model)
		VALUES (?, ?)
		ON DUPLICATE KEY UPDATE service_id=service_id`,
		sc.Vendor+"/"+sc.Model, sc.Model)
	if err != nil {
		return fmt.Errorf("model_services: %w", err)
	}
	msID, _ := res.LastInsertId()
	if msID == 0 {
		if err := db.GetContext(ctx, &msID, `SELECT id FROM model_services WHERE model=?`, sc.Model); err != nil {
			return fmt.Errorf("re-fetch model_service: %w", err)
		}
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO account_model_subscriptions (account_id, model_service_id)
		VALUES ('e2e-multivendor', ?)
		ON DUPLICATE KEY UPDATE account_id=account_id`, msID); err != nil {
		return fmt.Errorf("subscriptions: %w", err)
	}

	auth, err := repo.EncodePayload(sc.AuthType, sc.UpstreamAuth)
	if err != nil {
		return fmt.Errorf("encode auth: %w", err)
	}
	ep := &repo.Endpoint{
		Name:     "mv_" + sc.Vendor,
		Vendor:   sc.Vendor,
		Protocol: sc.Protocol,
		Model:    sc.Model,
		Group:    "default",
		Weight:   100,
		Enabled:  true,
		Auth:     auth,
		Routing:  repo.RoutingConfig{URL: mockBase + sc.UpstreamPath},
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

	hash := repo.HashAPIKey(sc.ClientAPIKey)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO api_keys
		  (account_id, api_key_hash, api_key_prefix, api_key_id,
		   sub_account_id, group_name, enabled)
		VALUES
		  ('e2e-multivendor', ?, ?, ?, ?, 'default', 1)
		ON DUPLICATE KEY UPDATE account_id=account_id`,
		hash, apiKeyPrefix(sc.ClientAPIKey), "ak_mv_"+sc.Vendor, sc.Vendor+"@e2e-multivendor"); err != nil {
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
