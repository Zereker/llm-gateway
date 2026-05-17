package domain

import "time"

// ModelService 全局模型 catalog 条目（docs/06 §3 业务结构）。
type ModelService struct {
	ID        int64
	ServiceID string
	Model     string

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}
