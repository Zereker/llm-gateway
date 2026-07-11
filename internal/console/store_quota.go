package console

import (
	"context"
	"encoding/json"
	"time"

	"github.com/zereker/llm-gateway/internal/ratelimit"
)

type QuotaPolicyInput struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Rule        json.RawMessage `json:"rule"`
}

type InvalidPolicyError struct{ Reason string }

func (e *InvalidPolicyError) Error() string { return "quota policy invalid: " + e.Reason }

func (s *Store) CreateQuotaPolicy(ctx context.Context, in QuotaPolicyInput) (int64, error) {
	if in.Name == "" {
		return 0, &InvalidPolicyError{Reason: "name required"}
	}
	if len(in.Rule) == 0 {
		return 0, &InvalidPolicyError{Reason: "rule required"}
	}
	var rule ratelimit.PolicyRule
	if err := json.Unmarshal(in.Rule, &rule); err != nil {
		return 0, &InvalidPolicyError{Reason: "rule not a valid PolicyRule: " + err.Error()}
	}
	if rule.Default == nil && len(rule.PerModel) == 0 {
		return 0, &InvalidPolicyError{Reason: "rule has neither default nor per_model"}
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO quota_policies (name, description, rule_json) VALUES (?, ?, ?)`,
		in.Name, in.Description, []byte(in.Rule))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

type QuotaPolicyView struct {
	ID          int64           `db:"id" json:"id"`
	Name        string          `db:"name" json:"name"`
	Description string          `db:"description" json:"description"`
	RuleJSON    json.RawMessage `db:"rule_json" json:"rule"`
	Enabled     bool            `db:"enabled" json:"enabled"`
	CreatedAt   time.Time       `db:"created_at" json:"created_at"`
}

func (s *Store) ListQuotaPolicies(ctx context.Context) ([]QuotaPolicyView, error) {
	var rows []QuotaPolicyView
	err := s.db.SelectContext(ctx, &rows,
		`SELECT id, name, description, rule_json, enabled, created_at
		 FROM quota_policies WHERE deleted_at IS NULL ORDER BY id`)
	return rows, err
}

func (s *Store) DeleteQuotaPolicy(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE quota_policies SET deleted_at = NOW(6) WHERE id = ? AND deleted_at IS NULL`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
