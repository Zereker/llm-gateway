package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/zereker/llm-gateway/internal/cachebus"
	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/policy"
)

type InvalidEnforcementPolicyError struct{ Reason string }

func (e *InvalidEnforcementPolicyError) Error() string {
	return "enforcement policy invalid: " + e.Reason
}

type EnforcementPolicyInput struct {
	PolicyID       string            `json:"policy_id,omitempty"`
	Name           string            `json:"name"`
	InputEnabled   bool              `json:"input_enabled"`
	OutputMode     policy.OutputMode `json:"output_mode"`
	MaxBufferBytes int               `json:"max_buffer_bytes,omitempty"`
}

type EnforcementPolicyView struct {
	PolicyID       string            `db:"policy_id" json:"policy_id"`
	Version        uint64            `db:"version" json:"version"`
	Name           string            `db:"name" json:"name"`
	InputEnabled   bool              `db:"input_enabled" json:"input_enabled"`
	OutputMode     policy.OutputMode `db:"output_mode" json:"output_mode"`
	MaxBufferBytes int               `db:"max_buffer_bytes" json:"max_buffer_bytes"`
	Enabled        bool              `db:"enabled" json:"enabled"`
	CreatedBy      string            `db:"created_by" json:"created_by,omitempty"`
	CreatedAt      time.Time         `db:"created_at" json:"created_at"`
}

func (s *Store) PublishEnforcementPolicy(ctx context.Context, in EnforcementPolicyInput, actor string) (EnforcementPolicyView, error) {
	if in.MaxBufferBytes == 0 {
		in.MaxBufferBytes = policy.DefaultMaxBufferBytes
	}

	if in.OutputMode == "" {
		in.OutputMode = policy.OutputDisabled
	}

	probe := policy.Definition{
		Ref:  policy.PolicyRef{ID: "validation", Version: 1, Scope: policy.Scope{Kind: policy.ScopeGlobal}},
		Name: in.Name, InputEnabled: in.InputEnabled, OutputMode: in.OutputMode, MaxBufferBytes: in.MaxBufferBytes,
	}
	if err := probe.Validate(); err != nil {
		return EnforcementPolicyView{}, &InvalidEnforcementPolicyError{Reason: err.Error()}
	}

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return EnforcementPolicyView{}, err
	}
	defer func() { _ = tx.Rollback() }()

	policyID := in.PolicyID

	version := uint64(1)
	if policyID == "" {
		policyID, err = generateID("pp_")
		if err != nil {
			return EnforcementPolicyView{}, err
		}
	} else {
		var current uint64

		err = tx.GetContext(ctx, &current,
			`SELECT version FROM policy_definitions WHERE policy_id = ? AND deleted_at IS NULL ORDER BY version DESC LIMIT 1 FOR UPDATE`, policyID)
		switch {
		case err == nil:
			version = current + 1
		case errors.Is(err, sql.ErrNoRows):
			version = 1
		default:
			return EnforcementPolicyView{}, err
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO policy_definitions
		 (policy_id, version, name, input_enabled, output_mode, max_buffer_bytes, enabled, created_by)
		 VALUES (?, ?, ?, ?, ?, ?, 1, ?)`,
		policyID, version, in.Name, in.InputEnabled, in.OutputMode, in.MaxBufferBytes, actor); err != nil {
		return EnforcementPolicyView{}, err
	}

	if err := tx.Commit(); err != nil {
		return EnforcementPolicyView{}, err
	}

	s.invalidatePolicies(ctx, policyID)

	return EnforcementPolicyView{
		PolicyID: policyID, Version: version, Name: in.Name, InputEnabled: in.InputEnabled,
		OutputMode: in.OutputMode, MaxBufferBytes: in.MaxBufferBytes, Enabled: true,
		CreatedBy: actor, CreatedAt: time.Now().UTC(),
	}, nil
}

func (s *Store) ListEnforcementPolicies(ctx context.Context) ([]EnforcementPolicyView, error) {
	var rows []EnforcementPolicyView

	err := s.db.SelectContext(ctx, &rows,
		`SELECT policy_id, version, name, input_enabled, output_mode, max_buffer_bytes,
		        enabled, created_by, created_at
		 FROM policy_definitions WHERE deleted_at IS NULL ORDER BY policy_id, version DESC`)

	return rows, err
}

func (s *Store) DisableEnforcementPolicy(ctx context.Context, policyID string) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `UPDATE policy_definitions SET enabled = 0 WHERE policy_id = ? AND enabled = 1`, policyID)
	if err != nil {
		return err
	}

	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE policy_bindings SET enabled = 0, deleted_at = NOW(6) WHERE policy_id = ? AND deleted_at IS NULL`, policyID); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	s.invalidatePolicies(ctx, policyID)

	return nil
}

type PolicyBindingInput struct {
	Scope         policy.Scope `json:"scope"`
	PolicyID      string       `json:"policy_id"`
	PolicyVersion uint64       `json:"policy_version"`
}

type PolicyBindingView struct {
	Scope         policy.Scope `json:"scope"`
	PolicyID      string       `json:"policy_id"`
	PolicyVersion uint64       `json:"policy_version"`
	Enabled       bool         `json:"enabled"`
	CreatedBy     string       `json:"created_by,omitempty"`
	CreatedAt     time.Time    `json:"created_at"`
	UpdatedAt     time.Time    `json:"updated_at"`
}

func (s *Store) BindEnforcementPolicy(ctx context.Context, in PolicyBindingInput, actor string) error {
	if err := validateBindingScope(in.Scope); err != nil {
		return err
	}

	if in.PolicyID == "" || in.PolicyVersion == 0 {
		return &InvalidEnforcementPolicyError{Reason: "policy_id and policy_version are required"}
	}

	var exists int
	if err := s.db.GetContext(ctx, &exists,
		`SELECT COUNT(*) FROM policy_definitions WHERE policy_id = ? AND version = ? AND enabled = 1 AND deleted_at IS NULL`,
		in.PolicyID, in.PolicyVersion); err != nil {
		return err
	}

	if exists == 0 {
		return &InvalidEnforcementPolicyError{Reason: "enabled policy version does not exist"}
	}

	switch in.Scope.Kind {
	case policy.ScopeAccount:
		if err := s.db.GetContext(ctx, &exists, `SELECT COUNT(*) FROM accounts WHERE pin = ? AND enabled = 1 AND deleted_at IS NULL`, in.Scope.ID); err != nil {
			return err
		}
	case policy.ScopeAPIKey:
		if err := s.db.GetContext(ctx, &exists, `SELECT COUNT(*) FROM api_keys WHERE api_key_id = ? AND enabled = 1 AND deleted_at IS NULL`, in.Scope.ID); err != nil {
			return err
		}
	}

	if in.Scope.Kind != policy.ScopeGlobal && exists == 0 {
		return &InvalidEnforcementPolicyError{Reason: "scope does not exist or is disabled"}
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO policy_bindings (scope_kind, scope_id, policy_id, policy_version, enabled, created_by)
		 VALUES (?, ?, ?, ?, 1, ?)
		 ON DUPLICATE KEY UPDATE policy_id = VALUES(policy_id), policy_version = VALUES(policy_version),
		 enabled = 1, created_by = VALUES(created_by), deleted_at = NULL`,
		in.Scope.Kind, in.Scope.ID, in.PolicyID, in.PolicyVersion, actor)
	if err == nil {
		s.invalidatePolicies(ctx, in.PolicyID)
	}

	return err
}

func validateBindingScope(scope policy.Scope) error {
	switch scope.Kind {
	case policy.ScopeGlobal:
		if scope.ID != "" {
			return &InvalidEnforcementPolicyError{Reason: "global scope ID must be empty"}
		}
	case policy.ScopeAccount, policy.ScopeAPIKey:
		if scope.ID == "" {
			return &InvalidEnforcementPolicyError{Reason: string(scope.Kind) + " scope ID is required"}
		}
	case policy.ScopeProject:
		return &InvalidEnforcementPolicyError{Reason: "project scope is reserved until trusted project identity and RBAC are available"}
	default:
		return &InvalidEnforcementPolicyError{Reason: "scope.kind must be global, account, or api_key"}
	}

	return nil
}

func (s *Store) ListPolicyBindings(ctx context.Context) ([]PolicyBindingView, error) {
	var rows []struct {
		ScopeKind     string    `db:"scope_kind"`
		ScopeID       string    `db:"scope_id"`
		PolicyID      string    `db:"policy_id"`
		PolicyVersion uint64    `db:"policy_version"`
		Enabled       bool      `db:"enabled"`
		CreatedBy     string    `db:"created_by"`
		CreatedAt     time.Time `db:"created_at"`
		UpdatedAt     time.Time `db:"updated_at"`
	}
	if err := s.db.SelectContext(ctx, &rows,
		`SELECT scope_kind, scope_id, policy_id, policy_version, enabled, created_by, created_at, updated_at
		 FROM policy_bindings WHERE deleted_at IS NULL ORDER BY scope_kind, scope_id`); err != nil {
		return nil, err
	}

	views := make([]PolicyBindingView, 0, len(rows))
	for _, row := range rows {
		views = append(views, PolicyBindingView{
			Scope:    policy.Scope{Kind: policy.ScopeKind(row.ScopeKind), ID: row.ScopeID},
			PolicyID: row.PolicyID, PolicyVersion: row.PolicyVersion, Enabled: row.Enabled,
			CreatedBy: row.CreatedBy, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		})
	}

	return views, nil
}

func (s *Store) DeletePolicyBinding(ctx context.Context, scope policy.Scope) error {
	if err := validateBindingScope(scope); err != nil {
		return err
	}

	res, err := s.db.ExecContext(ctx,
		`UPDATE policy_bindings SET enabled = 0, deleted_at = NOW(6) WHERE scope_kind = ? AND scope_id = ? AND deleted_at IS NULL`,
		scope.Kind, scope.ID)
	if err != nil {
		return err
	}

	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}

	s.invalidatePolicies(ctx, string(scope.Kind)+":"+scope.ID)

	return nil
}

type PolicySimulationInput struct {
	Protocol domain.Protocol          `json:"protocol"`
	Modality domain.Modality          `json:"modality"`
	Stage    policy.Stage             `json:"stage"`
	Body     json.RawMessage          `json:"body"`
	Decision PolicySimulationDecision `json:"decision"`
}

type PolicySimulationDecision struct {
	Action     policy.Action              `json:"action"`
	Policy     policy.PolicyRef           `json:"policy"`
	RuleID     string                     `json:"rule_id"`
	ReasonCode string                     `json:"reason_code"`
	Mutations  []PolicySimulationMutation `json:"mutations,omitempty"`
}

type PolicySimulationMutation struct {
	ID          string              `json:"id"`
	Kind        policy.MutationKind `json:"kind"`
	Target      string              `json:"target"`
	Replacement string              `json:"replacement"`
}

type PolicySimulationResult struct {
	Allowed bool               `json:"allowed"`
	Body    json.RawMessage    `json:"body,omitempty"`
	Audit   policy.AuditRecord `json:"audit"`
}

func (s *Store) SimulateEnforcementPolicy(_ context.Context, in PolicySimulationInput) (PolicySimulationResult, error) {
	if in.Stage != policy.StageInput && in.Stage != policy.StageOutput {
		return PolicySimulationResult{}, &InvalidEnforcementPolicyError{Reason: "stage must be input or output"}
	}

	decision := policy.Decision{
		Action: in.Decision.Action, Policy: in.Decision.Policy,
		RuleID: in.Decision.RuleID, ReasonCode: in.Decision.ReasonCode,
		Mutations: make([]policy.Mutation, 0, len(in.Decision.Mutations)),
	}
	for _, mutation := range in.Decision.Mutations {
		decision.Mutations = append(decision.Mutations, policy.Mutation{
			ID: mutation.ID, Kind: mutation.Kind, Target: mutation.Target, Replacement: []byte(mutation.Replacement),
		})
	}

	if err := decision.Validate(); err != nil {
		return PolicySimulationResult{}, &InvalidEnforcementPolicyError{Reason: err.Error()}
	}

	audit := decision.SafeAudit(in.Stage)
	switch decision.Action {
	case policy.ActionAllow:
		return PolicySimulationResult{Allowed: true, Body: in.Body, Audit: audit.WithEnforcement(policy.EnforcementAllowed)}, nil
	case policy.ActionDeny:
		return PolicySimulationResult{Allowed: false, Audit: audit.WithEnforcement(policy.EnforcementDenied)}, nil
	case policy.ActionRedact:
		body, err := (policy.JSONDocumentAdapter{}).Apply(in.Body, in.Protocol, in.Modality, decision.Mutations)
		if err != nil {
			return PolicySimulationResult{}, &InvalidEnforcementPolicyError{Reason: err.Error()}
		}

		return PolicySimulationResult{Allowed: true, Body: body, Audit: audit.WithEnforcement(policy.EnforcementApplied)}, nil
	}

	return PolicySimulationResult{}, &InvalidEnforcementPolicyError{Reason: "unsupported action"}
}

func (s *Store) invalidatePolicies(ctx context.Context, key string) {
	if err := s.pub.Invalidate(ctx, cachebus.Invalidation{Kind: cachebus.KindPolicy, Key: key}); err != nil {
		slog.WarnContext(ctx, "policy cache invalidation failed; data plane will fall back to TTL", "err", err)
	}
}
