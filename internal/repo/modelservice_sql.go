package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// SQLModelServiceReader is the sqlx implementation of ModelServiceReader.
//
// **v0.3 change**: dropped account_id (model_services is now a global
// catalog); GetByModel's signature dropped the accountID parameter.
type SQLModelServiceReader struct {
	db *sqlx.DB
}

// NewSQLModelServiceReader builds one from an existing *sqlx.DB (opens no new connection).
func NewSQLModelServiceReader(db *sqlx.DB) *SQLModelServiceReader {
	return &SQLModelServiceReader{db: db}
}

const msColumns = `id, service_id, model, created_at, updated_at, deleted_at`

// GetByModel implements ModelServiceReader.GetByModel; queries globally by model.
//
// **Alias resolution**: when a direct query on model_services misses, it
// tries once more via model_aliases, redirecting the alias to the canonical
// model. Aliasing is transparent to downstream consumers — the returned value
// is the canonical ModelService, and subscription / routing / metering all
// proceed against the canonical model afterward.
func (r *SQLModelServiceReader) GetByModel(ctx context.Context, model string) (*ModelService, error) {
	if model == "" {
		return nil, errors.New("model_service: empty model name")
	}

	var ms ModelService

	err := r.db.GetContext(ctx, &ms, r.db.Rebind(
		`SELECT `+msColumns+` FROM model_services
		 WHERE model = ? AND deleted_at IS NULL`),
		model)
	if err == nil {
		return &ms, nil
	}

	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("model_service: get by model: %w", err)
	}

	// Direct query missed -- try alias redirection (alias -> canonical model -> model_services).
	var aliased ModelService

	aerr := r.db.GetContext(ctx, &aliased, r.db.Rebind(
		`SELECT `+prefixCols("ms", msColumns)+`
		 FROM model_aliases a
		 JOIN model_services ms ON ms.model = a.model AND ms.deleted_at IS NULL
		 WHERE a.alias = ? AND a.enabled = 1 AND a.deleted_at IS NULL`),
		model)
	if aerr != nil {
		// No alias either -- genuinely not found (return nil, nil so M5 takes
		// the 404 path); only a DB failure gets wrapped.
		if errors.Is(aerr, sql.ErrNoRows) {
			return nil, nil
		}

		return nil, fmt.Errorf("model_service: alias resolve: %w", aerr)
	}

	return &aliased, nil
}

// prefixCols adds a table-alias prefix to each comma-separated column name
// ("id, model" -> "ms.id, ms.model") — used to disambiguate JOIN queries.
func prefixCols(prefix, cols string) string {
	var b []byte

	field := make([]byte, 0, 16)

	flush := func() {
		f := trimSpace(string(field))
		if f != "" {
			if len(b) > 0 {
				b = append(b, ',', ' ')
			}

			b = append(b, prefix...)
			b = append(b, '.')
			b = append(b, f...)
		}

		field = field[:0]
	}
	for i := 0; i < len(cols); i++ {
		if cols[i] == ',' {
			flush()
			continue
		}

		field = append(field, cols[i])
	}

	flush()

	return string(b)
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\n' || s[start] == '\t') {
		start++
	}

	for end > start && (s[end-1] == ' ' || s[end-1] == '\n' || s[end-1] == '\t') {
		end--
	}

	return s[start:end]
}

// List implements ModelServiceReader.List; lists all non-deleted records (the global catalog).
func (r *SQLModelServiceReader) List(ctx context.Context) ([]*ModelService, error) {
	var rows []ModelService
	if err := r.db.SelectContext(ctx, &rows,
		`SELECT `+msColumns+` FROM model_services
		 WHERE deleted_at IS NULL ORDER BY id`,
	); err != nil {
		return nil, fmt.Errorf("model_service: list: %w", err)
	}

	out := make([]*ModelService, len(rows))
	for i := range rows {
		out[i] = &rows[i]
	}

	return out, nil
}

// Compile-time assertion.
var _ ModelServiceReader = (*SQLModelServiceReader)(nil)
