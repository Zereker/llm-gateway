package repo

import (
	"context"

	"github.com/zereker/llm-gateway/internal/domain"
)

// DomainModelReader exposes the model catalog in domain terms.
type DomainModelReader interface {
	GetByModel(ctx context.Context, model string) (*domain.ModelService, error)
}

type domainModelReader struct{ rows ModelServiceReader }

func NewDomainModelReader(rows ModelServiceReader) DomainModelReader {
	return domainModelReader{rows: rows}
}

func (r domainModelReader) GetByModel(ctx context.Context, model string) (*domain.ModelService, error) {
	row, err := r.rows.GetByModel(ctx, model)
	if err != nil {
		return nil, err
	}
	return ToDomainModelService(row), nil
}

// DomainEndpointReader exposes endpoint queries in domain terms.
type DomainEndpointReader interface {
	ListForModel(ctx context.Context, model, group string) ([]*domain.Endpoint, error)
	List(ctx context.Context) ([]*domain.Endpoint, error)
}

type domainEndpointReader struct{ rows EndpointReader }

func NewDomainEndpointReader(rows EndpointReader) DomainEndpointReader {
	return domainEndpointReader{rows: rows}
}

func (r domainEndpointReader) ListForModel(ctx context.Context, model, group string) ([]*domain.Endpoint, error) {
	rows, err := r.rows.ListForModel(ctx, model, group)
	if err != nil {
		return nil, err
	}
	return ToDomainEndpoints(rows), nil
}

func (r domainEndpointReader) List(ctx context.Context) ([]*domain.Endpoint, error) {
	rows, err := r.rows.List(ctx)
	if err != nil {
		return nil, err
	}
	return ToDomainEndpoints(rows), nil
}
