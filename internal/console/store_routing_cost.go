package console

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/zereker/llm-gateway/internal/cachebus"
)

type RoutingCostInput struct {
	ProfileID                     string `json:"profile_id,omitempty"`
	ModelServiceID                int64  `json:"model_service_id"`
	InputMicrousdPerMillionToken  uint64 `json:"input_microusd_per_million_tokens"`
	OutputMicrousdPerMillionToken uint64 `json:"output_microusd_per_million_tokens"`
}

type RoutingCostView struct {
	ProfileID                     string    `db:"profile_id" json:"profile_id"`
	Version                       uint64    `db:"version" json:"version"`
	ModelServiceID                int64     `db:"model_service_id" json:"model_service_id"`
	Model                         string    `db:"model" json:"model"`
	InputMicrousdPerMillionToken  uint64    `db:"input_microusd_per_million_tokens" json:"input_microusd_per_million_tokens"`
	OutputMicrousdPerMillionToken uint64    `db:"output_microusd_per_million_tokens" json:"output_microusd_per_million_tokens"`
	Enabled                       bool      `db:"enabled" json:"enabled"`
	CreatedBy                     string    `db:"created_by" json:"created_by,omitempty"`
	CreatedAt                     time.Time `db:"created_at" json:"created_at"`
}

type InvalidRoutingCostError struct{ Reason string }

func (e *InvalidRoutingCostError) Error() string { return "routing cost invalid: " + e.Reason }

func (s *Store) PublishRoutingCost(ctx context.Context, in RoutingCostInput, actor string) (RoutingCostView, error) {
	if in.ModelServiceID == 0 || (in.InputMicrousdPerMillionToken == 0 && in.OutputMicrousdPerMillionToken == 0) {
		return RoutingCostView{}, &InvalidRoutingCostError{Reason: "model_service_id and at least one non-zero token cost are required"}
	}

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return RoutingCostView{}, err
	}

	defer func() { _ = tx.Rollback() }()

	var model string
	if err := tx.GetContext(ctx, &model, `SELECT model FROM model_services WHERE id = ? AND deleted_at IS NULL FOR UPDATE`, in.ModelServiceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RoutingCostView{}, &InvalidRoutingCostError{Reason: "model service does not exist"}
		}

		return RoutingCostView{}, err
	}

	var current struct {
		ProfileID string `db:"profile_id"`
		Version   uint64 `db:"version"`
	}

	err = tx.GetContext(ctx, &current, `SELECT profile_id, version FROM routing_cost_profiles
		WHERE model_service_id = ? AND deleted_at IS NULL ORDER BY version DESC LIMIT 1 FOR UPDATE`, in.ModelServiceID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return RoutingCostView{}, err
	}

	if errors.Is(err, sql.ErrNoRows) {
		current.ProfileID = in.ProfileID
		if current.ProfileID == "" {
			current.ProfileID, err = generateID("rcp_")
			if err != nil {
				return RoutingCostView{}, err
			}
		}
	} else if in.ProfileID != "" && in.ProfileID != current.ProfileID {
		return RoutingCostView{}, &InvalidRoutingCostError{Reason: "profile_id does not own this model"}
	}

	version := current.Version + 1

	if _, err := tx.ExecContext(ctx, `UPDATE routing_cost_profiles SET enabled = 0 WHERE model_service_id = ? AND enabled = 1`, in.ModelServiceID); err != nil {
		return RoutingCostView{}, err
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO routing_cost_profiles
		(profile_id, version, model_service_id, input_microusd_per_million_tokens,
		 output_microusd_per_million_tokens, enabled, created_by)
		VALUES (?, ?, ?, ?, ?, 1, ?)`, current.ProfileID, version, in.ModelServiceID,
		in.InputMicrousdPerMillionToken, in.OutputMicrousdPerMillionToken, actor); err != nil {
		return RoutingCostView{}, err
	}

	if err := tx.Commit(); err != nil {
		return RoutingCostView{}, err
	}

	if err := s.pub.Invalidate(ctx, cachebus.Invalidation{Kind: cachebus.KindRoutingCost, Key: fmt.Sprintf("%d", in.ModelServiceID)}); err != nil {
		slog.WarnContext(ctx, "routing cost cache invalidation failed; data plane will fall back to TTL", "err", err)
	}

	return RoutingCostView{ProfileID: current.ProfileID, Version: version, ModelServiceID: in.ModelServiceID,
		Model: model, InputMicrousdPerMillionToken: in.InputMicrousdPerMillionToken,
		OutputMicrousdPerMillionToken: in.OutputMicrousdPerMillionToken, Enabled: true,
		CreatedBy: actor, CreatedAt: time.Now().UTC()}, nil
}

func (s *Store) ListRoutingCosts(ctx context.Context) ([]RoutingCostView, error) {
	var rows []RoutingCostView

	err := s.db.SelectContext(ctx, &rows, `SELECT p.profile_id, p.version, p.model_service_id, m.model,
		p.input_microusd_per_million_tokens, p.output_microusd_per_million_tokens,
		p.enabled, p.created_by, p.created_at
		FROM routing_cost_profiles p JOIN model_services m ON m.id = p.model_service_id
		WHERE p.deleted_at IS NULL ORDER BY m.model, p.version DESC`)

	return rows, err
}
