package domain

import "time"

// ModelService is a global model catalog entry (docs/06 §3 business struct).
type ModelService struct {
	ID        int64
	ServiceID string
	Model     string

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}
