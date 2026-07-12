// middleware_adapters.go: a small adapter layer in the composition root.
//
// Why not in internal/repo: it would form a cycle (middleware → ratelimit → repo →
// middleware). An adapter is wiring glue code, and it naturally belongs in
// the composition root—every upstream
// and downstream package is already imported there, so there's zero cycle.
//
// This file only holds "repo row type → middleware port" shape conversions;
// no business logic goes here.
package gateway

import (
	"context"

	"github.com/zereker/llm-gateway/internal/middleware"
	"github.com/zereker/llm-gateway/internal/ratelimit"
	"github.com/zereker/llm-gateway/internal/repo"
)

// adaptSubscriptions adapts SubscriptionProvider to middleware.SubscriptionChecker.
func adaptSubscriptions(p repo.SubscriptionProvider) middleware.SubscriptionChecker {
	return repoSubsAdapter{p: p}
}

type repoSubsAdapter struct{ p repo.SubscriptionProvider }

func (a repoSubsAdapter) HasModel(ctx context.Context, accountID string, modelServiceID int64) (bool, error) {
	return a.p.Has(ctx, accountID, modelServiceID)
}

// adaptQuotaPolicies keeps ratelimit independent of SQL row types.
func adaptQuotaPolicies(p repo.QuotaPolicyProvider) ratelimit.PolicySource {
	return repoQuotaPolicyAdapter{p: p}
}

type repoQuotaPolicyAdapter struct{ p repo.QuotaPolicyProvider }

func (a repoQuotaPolicyAdapter) RuleJSONByID(ctx context.Context, id int64) ([]byte, error) {
	policy, err := a.p.GetByID(ctx, id)
	if err != nil || policy == nil {
		return nil, err
	}

	return policy.RuleJSON, nil
}

// Compile-time port satisfaction assertions—verifies that the concrete types
// referenced at wiring points satisfy the middleware port. Placed in
// internal/app/gateway rather than internal/repo / internal/ratelimit to avoid an import cycle
// (see this file's doc comment).
var (
	// internal/repo
	_ middleware.IdentityProvider = (*repo.SQLAPIKeyProvider)(nil)
)
