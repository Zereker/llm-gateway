package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// SQLModelServiceReader 是 ModelServiceReader 的 sqlx 实现。
//
// **v0.3 改动**：去 account_id（model_services 是全局 catalog）；GetByModel 签名去 accountID 参数。
type SQLModelServiceReader struct {
	db *sqlx.DB
}

// NewSQLModelServiceReader 用现成的 *sqlx.DB 构造（不打开新连接）。
func NewSQLModelServiceReader(db *sqlx.DB) *SQLModelServiceReader {
	return &SQLModelServiceReader{db: db}
}

const msColumns = `id, service_id, model, created_at, updated_at, deleted_at`

// GetByModel 实现 ModelServiceReader.GetByModel；按 model 全局查。
//
// **别名解析**：直查 model_services miss 时，再经 model_aliases 把 alias 重定向到
// canonical model 查一次。别名对下游透明——返回的是 canonical ModelService，之后
// 订阅 / 选路 / 计量全按 canonical model 走。
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

	// 直查 miss —— 试别名重定向（alias → canonical model → model_services）。
	var aliased ModelService
	aerr := r.db.GetContext(ctx, &aliased, r.db.Rebind(
		`SELECT `+prefixCols("ms", msColumns)+`
		 FROM model_aliases a
		 JOIN model_services ms ON ms.model = a.model AND ms.deleted_at IS NULL
		 WHERE a.alias = ? AND a.enabled = 1 AND a.deleted_at IS NULL`),
		model)
	if aerr != nil {
		// 别名也没有 —— 真 not found（返 nil,nil 让 M5 走 404）；DB 故障才 wrap。
		if errors.Is(aerr, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("model_service: alias resolve: %w", aerr)
	}
	return &aliased, nil
}

// prefixCols 给逗号分隔列名统一加表别名前缀（"id, model" → "ms.id, ms.model"）——
// JOIN 查询里消歧用。
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

// List 实现 ModelServiceReader.List；列全部未删 records（全局 catalog）。
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

// 编译期断言。
var _ ModelServiceReader = (*SQLModelServiceReader)(nil)
