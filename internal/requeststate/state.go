// Package requeststate contains the mutable state carried by the HTTP
// middleware pipeline. It intentionally lives outside domain: transport
// orchestration state is not a business entity.
package requeststate

import (
	"time"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/protocol"
)

// State is populated progressively by the gateway middleware chain.
type State struct {
	RequestID string
	StartTime time.Time

	Identity domain.UserIdentity
	Envelope *domain.RequestEnvelope
	Handlers protocol.Lookup

	ModelService       *domain.ModelService
	ModelChain         []*domain.ModelService
	RoutedModelService *domain.ModelService
	Endpoint           *domain.Endpoint

	Usage                *domain.Usage
	Error                *domain.AdapterError
	ModelRoutingDecision *domain.ModelRoutingDecision
	SchedulingDecision   *domain.SchedulingDecision
}
