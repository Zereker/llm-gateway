// middleware_adapters.go：composition root 的小适配层。
//
// 为什么不在 pkg/repo 里：会形成 cycle（middleware → ratelimit → repo →
// middleware）。adapter 是装配粘合代码，本来就应该住在 composition root
// （cmd/gateway/main.go 同包）——所有上下游包都已 import 到位，零循环。
//
// 这里只放"repo 行类型 → middleware port"的形状转换；不放业务逻辑。
package main

import (
	"context"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/middleware"
	"github.com/zereker/llm-gateway/pkg/repo"
	"github.com/zereker/llm-gateway/pkg/selector"
)

// adaptCatalog 把 SQL 行型 ModelServiceReader 适配为 middleware.ModelCatalog。
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

// adaptSubscriptions 把 SubscriptionProvider 适配为 middleware.SubscriptionChecker。
func adaptSubscriptions(p repo.SubscriptionProvider) middleware.SubscriptionChecker {
	return repoSubsAdapter{p: p}
}

type repoSubsAdapter struct{ p repo.SubscriptionProvider }

func (a repoSubsAdapter) HasModel(ctx context.Context, accountID string, modelServiceID int64) (bool, error) {
	return a.p.Has(ctx, accountID, modelServiceID)
}

// adaptEndpoints 把 SQL 行型 EndpointReader 适配为 selector.EndpointReader。
func adaptEndpoints(p repo.EndpointReader) selector.EndpointReader {
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

// Compile-time port satisfaction assertions——验证装配点引用的 concrete 类型
// 满足 middleware port。放在 cmd/gateway 而不是 pkg/repo / pkg/ratelimit 是为了
// 避免 import cycle（详见本文件 doc）。
var (
	// pkg/repo
	_ middleware.IdentityProvider = (*repo.SQLAPIKeyProvider)(nil)
)
