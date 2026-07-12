package repo

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// CheckSchema verifies the business tables exist and are readable — a
// defensive check run at gateway startup right after infra.Migrate (surfaces
// a missed schema.sql change early, with a friendlier error than "SQL: no
// such table").
//
// Reads no data, only issues prepare-level queries. The gateway can still
// start when the tables are empty; once requests arrive, M5 / M7 / M2 return
// 404 / 503 / 401 respectively (the deployer manages data directly via SQL).
func CheckSchema(ctx context.Context, db *sqlx.DB) error {
	for _, t := range []string{
		"quota_policies",
		"accounts",
		"model_services",
		"endpoints",
		"account_model_subscriptions",
		"api_keys",
		"pricing_versions",
	} {
		if _, err := db.ExecContext(ctx, "SELECT 1 FROM "+t+" LIMIT 0"); err != nil {
			return fmt.Errorf("repo: schema check failed on %q (run infra.Migrate or apply schema.sql first): %w", t, err)
		}
	}

	return nil
}
