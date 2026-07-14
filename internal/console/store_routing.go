package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/zereker/llm-gateway/internal/cachebus"
	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/repo"
	"github.com/zereker/llm-gateway/internal/routingpolicy"
)

type RoutingPolicyInput struct {
	PolicyID     string                          `json:"policy_id,omitempty"`
	Scope        domain.RoutingScope             `json:"scope"`
	VirtualModel string                          `json:"virtual_model"`
	MaxAttempts  int                             `json:"max_attempts,omitempty"`
	Constraints  domain.RoutingConstraints       `json:"constraints,omitempty"`
	Candidates   []domain.RoutingPolicyCandidate `json:"candidates"`
}

type InvalidRoutingPolicyError struct{ Reason string }

func (e *InvalidRoutingPolicyError) Error() string { return "routing policy invalid: " + e.Reason }

type RoutingPolicyView struct {
	PolicyID     string                          `json:"policy_id"`
	Version      uint64                          `json:"version"`
	Scope        domain.RoutingScope             `json:"scope"`
	VirtualModel string                          `json:"virtual_model"`
	MaxAttempts  int                             `json:"max_attempts,omitempty"`
	Constraints  domain.RoutingConstraints       `json:"constraints,omitempty"`
	Candidates   []domain.RoutingPolicyCandidate `json:"candidates"`
	Enabled      bool                            `json:"enabled"`
	CreatedBy    string                          `json:"created_by,omitempty"`
	CreatedAt    time.Time                       `json:"created_at"`
}

type routingPolicyStoreRow struct {
	PolicyID     string          `db:"policy_id"`
	Version      uint64          `db:"version"`
	ScopeKind    string          `db:"scope_kind"`
	ScopeID      string          `db:"scope_id"`
	VirtualModel string          `db:"virtual_model"`
	RuleJSON     json.RawMessage `db:"rule_json"`
	Enabled      bool            `db:"enabled"`
	CreatedBy    string          `db:"created_by"`
	CreatedAt    time.Time       `db:"created_at"`
}

type routingPolicyStoreRule struct {
	MaxAttempts int                             `json:"max_attempts,omitempty"`
	Constraints domain.RoutingConstraints       `json:"constraints,omitempty"`
	Candidates  []domain.RoutingPolicyCandidate `json:"candidates"`
}

func (s *Store) PublishRoutingPolicy(ctx context.Context, in RoutingPolicyInput, actor string) (RoutingPolicyView, error) {
	if err := s.validateRoutingPolicyInput(ctx, in); err != nil {
		return RoutingPolicyView{}, err
	}

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return RoutingPolicyView{}, err
	}
	defer func() { _ = tx.Rollback() }()

	policyID, version, err := nextRoutingPolicyVersion(ctx, tx, in)
	if err != nil {
		return RoutingPolicyView{}, err
	}

	if in.PolicyID != "" && in.PolicyID != policyID {
		return RoutingPolicyView{}, &InvalidRoutingPolicyError{Reason: "policy_id does not own this scope and virtual model"}
	}

	if in.PolicyID == "" {
		in.PolicyID = policyID
	}

	rule, err := json.Marshal(routingPolicyStoreRule{
		MaxAttempts: in.MaxAttempts,
		Constraints: in.Constraints,
		Candidates:  in.Candidates,
	})
	if err != nil {
		return RoutingPolicyView{}, err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE routing_policies SET enabled = 0
		 WHERE scope_kind = ? AND scope_id = ? AND virtual_model = ? AND enabled = 1`,
		in.Scope.Kind, in.Scope.ID, in.VirtualModel); err != nil {
		return RoutingPolicyView{}, err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO routing_policies
		 (policy_id, version, scope_kind, scope_id, virtual_model, rule_json, enabled, created_by)
		 VALUES (?, ?, ?, ?, ?, ?, 1, ?)`,
		in.PolicyID, version, in.Scope.Kind, in.Scope.ID, in.VirtualModel, rule, actor); err != nil {
		return RoutingPolicyView{}, err
	}

	if err := tx.Commit(); err != nil {
		return RoutingPolicyView{}, err
	}

	if err := s.pub.Invalidate(ctx, cachebus.Invalidation{
		Kind: cachebus.KindRoutingPolicy,
		Key:  fmt.Sprintf("%s@%d", in.PolicyID, version),
	}); err != nil {
		slog.WarnContext(ctx, "routing policy cache invalidation failed; data plane will fall back to TTL", "err", err)
	}

	return RoutingPolicyView{
		PolicyID: in.PolicyID, Version: version, Scope: in.Scope, VirtualModel: in.VirtualModel,
		MaxAttempts: in.MaxAttempts, Constraints: in.Constraints, Candidates: in.Candidates,
		Enabled: true, CreatedBy: actor, CreatedAt: time.Now().UTC(),
	}, nil
}

func nextRoutingPolicyVersion(
	ctx context.Context,
	tx *sqlx.Tx,
	in RoutingPolicyInput,
) (string, uint64, error) {
	var current struct {
		PolicyID string `db:"policy_id"`
		Version  uint64 `db:"version"`
	}

	err := tx.GetContext(ctx, &current,
		`SELECT policy_id, version FROM routing_policies
		 WHERE scope_kind = ? AND scope_id = ? AND virtual_model = ? AND deleted_at IS NULL
		 ORDER BY version DESC LIMIT 1 FOR UPDATE`,
		in.Scope.Kind, in.Scope.ID, in.VirtualModel)
	if err == nil {
		return current.PolicyID, current.Version + 1, nil
	}

	if !errors.Is(err, sql.ErrNoRows) {
		return "", 0, err
	}

	policyID := in.PolicyID
	if policyID == "" {
		policyID, err = generateID("rp_")
		if err != nil {
			return "", 0, err
		}
	}

	return policyID, 1, nil
}

func (s *Store) validateRoutingPolicyInput(ctx context.Context, in RoutingPolicyInput) error {
	if in.VirtualModel == "" || len(in.Candidates) == 0 {
		return &InvalidRoutingPolicyError{Reason: "virtual_model and candidates are required"}
	}

	if in.MaxAttempts < 0 {
		return &InvalidRoutingPolicyError{Reason: "max_attempts cannot be negative"}
	}

	switch in.Scope.Kind {
	case domain.RoutingScopeGlobal:
		if in.Scope.ID != "" {
			return &InvalidRoutingPolicyError{Reason: "global scope_id must be empty"}
		}
	case domain.RoutingScopeAccount:
		if in.Scope.ID == "" {
			return &InvalidRoutingPolicyError{Reason: "account scope_id is required"}
		}

		var accountExists int
		if err := s.db.GetContext(ctx, &accountExists,
			`SELECT COUNT(*) FROM accounts WHERE pin = ? AND enabled = 1 AND deleted_at IS NULL`, in.Scope.ID); err != nil {
			return err
		}

		if accountExists == 0 {
			return &InvalidRoutingPolicyError{Reason: "account scope does not exist or is disabled: " + in.Scope.ID}
		}
	case domain.RoutingScopeProject:
		return &InvalidRoutingPolicyError{Reason: "project scope is reserved until trusted project identity and RBAC are available"}
	default:
		return &InvalidRoutingPolicyError{Reason: "scope.kind must be global or account"}
	}

	var collision int
	if err := s.db.GetContext(ctx, &collision,
		`SELECT (SELECT COUNT(*) FROM model_services WHERE model = ? AND deleted_at IS NULL) +
		        (SELECT COUNT(*) FROM model_aliases WHERE alias = ? AND enabled = 1 AND deleted_at IS NULL)`,
		in.VirtualModel, in.VirtualModel); err != nil {
		return err
	}

	if collision > 0 {
		return &InvalidRoutingPolicyError{Reason: "virtual_model collides with a concrete model or alias"}
	}

	seen := make(map[string]struct{}, len(in.Candidates))
	for _, candidate := range in.Candidates {
		if candidate.Model == "" {
			return &InvalidRoutingPolicyError{Reason: "candidate model is required"}
		}

		if _, duplicate := seen[candidate.Model]; duplicate {
			return &InvalidRoutingPolicyError{Reason: "duplicate candidate: " + candidate.Model}
		}

		seen[candidate.Model] = struct{}{}

		var exists int
		if err := s.db.GetContext(ctx, &exists,
			`SELECT COUNT(*) FROM model_services WHERE model = ? AND deleted_at IS NULL`, candidate.Model); err != nil {
			return err
		}

		if exists == 0 {
			return &InvalidRoutingPolicyError{Reason: "candidate model does not exist: " + candidate.Model}
		}
	}

	return nil
}

func (s *Store) ListRoutingPolicies(ctx context.Context) ([]RoutingPolicyView, error) {
	var rows []routingPolicyStoreRow
	if err := s.db.SelectContext(ctx, &rows,
		`SELECT policy_id, version, scope_kind, scope_id, virtual_model, rule_json,
		        enabled, created_by, created_at
		 FROM routing_policies WHERE deleted_at IS NULL
		 ORDER BY virtual_model, scope_kind, scope_id, version DESC`); err != nil {
		return nil, err
	}

	views := make([]RoutingPolicyView, 0, len(rows))
	for _, row := range rows {
		var rule routingPolicyStoreRule
		if err := json.Unmarshal(row.RuleJSON, &rule); err != nil {
			return nil, fmt.Errorf("decode routing policy %s@%d: %w", row.PolicyID, row.Version, err)
		}

		views = append(views, RoutingPolicyView{
			PolicyID: row.PolicyID, Version: row.Version,
			Scope:        domain.RoutingScope{Kind: domain.RoutingScopeKind(row.ScopeKind), ID: row.ScopeID},
			VirtualModel: row.VirtualModel, MaxAttempts: rule.MaxAttempts,
			Constraints: rule.Constraints, Candidates: rule.Candidates,
			Enabled: row.Enabled, CreatedBy: row.CreatedBy, CreatedAt: row.CreatedAt,
		})
	}

	return views, nil
}

func (s *Store) DisableRoutingPolicy(ctx context.Context, policyID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE routing_policies SET enabled = 0 WHERE policy_id = ? AND enabled = 1 AND deleted_at IS NULL`, policyID)
	if err != nil {
		return err
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}

	if affected == 0 {
		return ErrNotFound
	}

	if err := s.pub.Invalidate(ctx, cachebus.Invalidation{Kind: cachebus.KindRoutingPolicy, Key: policyID}); err != nil {
		slog.WarnContext(ctx, "routing policy cache invalidation failed; data plane will fall back to TTL", "err", err)
	}

	return nil
}

type RoutingDryRunInput struct {
	AccountID      string           `json:"account_id"`
	ProjectID      string           `json:"project_id,omitempty"`
	RequestedModel string           `json:"requested_model"`
	Region         string           `json:"region,omitempty"`
	Modality       *domain.Modality `json:"modality"`
}

func (s *Store) DryRunRoutingPolicy(ctx context.Context, in RoutingDryRunInput) (routingpolicy.Resolution, error) {
	if in.Modality == nil {
		return routingpolicy.Resolution{}, &InvalidRoutingPolicyError{Reason: "modality is required"}
	}

	catalog := repo.NewDomainModelReader(repo.NewSQLModelServiceReader(s.db))
	subscriptions := consoleSubscriptionChecker{inner: repo.NewSQLSubscriptionProvider(s.db)}
	resolver := routingpolicy.NewResolver(repo.NewSQLRoutingPolicyReader(s.db), catalog, subscriptions)

	return resolver.Resolve(ctx, routingpolicy.Input{
		RequestedModel: in.RequestedModel,
		AccountID:      in.AccountID,
		ProjectID:      in.ProjectID,
		Region:         in.Region,
		Modality:       *in.Modality,
	})
}

type consoleSubscriptionChecker struct{ inner repo.SubscriptionProvider }

func (c consoleSubscriptionChecker) HasModel(ctx context.Context, accountID string, modelServiceID int64) (bool, error) {
	return c.inner.Has(ctx, accountID, modelServiceID)
}
