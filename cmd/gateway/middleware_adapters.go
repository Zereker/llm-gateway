// middleware_adapters.go: a small adapter layer in the composition root.
//
// Why not in pkg/repo: it would form a cycle (middleware → ratelimit → repo →
// middleware). An adapter is wiring glue code, and it naturally belongs in
// the composition root (same package as cmd/gateway/main.go)—every upstream
// and downstream package is already imported there, so there's zero cycle.
//
// This file only holds "repo row type → middleware port" shape conversions;
// no business logic goes here.
package main

import (
	"context"

	"github.com/zereker/llm-gateway/pkg/dispatch"
	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/middleware"
	"github.com/zereker/llm-gateway/pkg/repo"
)

// adaptCatalog adapts the SQL row-based ModelServiceReader to middleware.ModelCatalog.
func adaptCatalog(p repo.ModelServiceReader) middleware.ModelCatalog {
	return repoCatalogAdapter{p: p}
}

type repoCatalogAdapter struct{ p repo.ModelServiceReader }

func (a repoCatalogAdapter) GetByModel(ctx context.Context, model string) (*domain.ModelService, error) {
	ms, err := a.p.GetByModel(ctx, model)
	if err != nil {
		return nil, err
	}
	return repo.ToDomainModelService(ms), nil
}

// adaptSubscriptions adapts SubscriptionProvider to middleware.SubscriptionChecker.
func adaptSubscriptions(p repo.SubscriptionProvider) middleware.SubscriptionChecker {
	return repoSubsAdapter{p: p}
}

type repoSubsAdapter struct{ p repo.SubscriptionProvider }

func (a repoSubsAdapter) HasModel(ctx context.Context, accountID string, modelServiceID int64) (bool, error) {
	return a.p.Has(ctx, accountID, modelServiceID)
}

// adaptEndpoints adapts the SQL row-based EndpointReader to dispatch.CandidateSource.
func adaptEndpoints(p repo.EndpointReader) dispatch.CandidateSource {
	return repoEndpointAdapter{p: p}
}

type repoEndpointAdapter struct{ p repo.EndpointReader }

func (a repoEndpointAdapter) ListForModel(ctx context.Context, model, group string) ([]*domain.Endpoint, error) {
	rows, err := a.p.ListForModel(ctx, model, group)
	if err != nil {
		return nil, err
	}
	return repo.ToDomainEndpoints(rows), nil
}

// Compile-time port satisfaction assertions—verifies that the concrete types
// referenced at wiring points satisfy the middleware port. Placed in
// cmd/gateway rather than pkg/repo / pkg/ratelimit to avoid an import cycle
// (see this file's doc comment).
var (
	// pkg/repo
	_ middleware.IdentityProvider = (*repo.SQLAPIKeyProvider)(nil)
)
