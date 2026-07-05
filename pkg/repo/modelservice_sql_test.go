package repo

import (
	"context"
	"testing"

	"github.com/jmoiron/sqlx"
)

// seedModelService writes test data into the db.
//
// **v0.3 change**: model_services dropped account_id/group_name/spec_detail.
func seedModelService(t *testing.T, db *sqlx.DB, ms *ModelService) {
	t.Helper()
	res, err := db.NamedExec(
		`INSERT INTO model_services (service_id, model)
		 VALUES (:service_id, :model)`,
		ms,
	)
	if err != nil {
		t.Fatalf("seed model_service: %v", err)
	}
	if id, err := res.LastInsertId(); err == nil {
		ms.ID = id
	}
}

func TestSQLModelServiceReader_GetByModel(t *testing.T) {
	db := newTestDB(t)
	seedModelService(t, db, &ModelService{
		ServiceID: "openai/gpt-4o",
		Model:     "gpt-4o",
	})

	r := NewSQLModelServiceReader(db)
	got, err := r.GetByModel(context.Background(), "gpt-4o")
	if err != nil {
		t.Fatalf("GetByModel: %v", err)
	}
	if got.ServiceID != "openai/gpt-4o" {
		t.Errorf("got %+v", got)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt not populated by DB DEFAULT")
	}
}

func TestSQLModelServiceReader_GetNotFound(t *testing.T) {
	r := NewSQLModelServiceReader(newTestDB(t))
	// docs/01 §7: not-found returns (nil, nil) so M5 takes its own 404 path;
	// only a SQL error returns err (fail-closed 503).
	ms, err := r.GetByModel(context.Background(), "missing")
	if err != nil {
		t.Fatalf("not-found should not be an error: %v", err)
	}
	if ms != nil {
		t.Fatalf("not-found should return nil ms, got %+v", ms)
	}
	if _, err := r.GetByModel(context.Background(), ""); err == nil {
		t.Fatal("want error for empty model name (input validation)")
	}
}

func TestSQLModelServiceReader_List(t *testing.T) {
	db := newTestDB(t)
	for _, m := range []string{"a", "b", "c"} {
		seedModelService(t, db, &ModelService{ServiceID: "v/" + m, Model: m})
	}

	r := NewSQLModelServiceReader(db)
	all, err := r.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("len = %d, want 3", len(all))
	}
}

func TestSQLModelServiceReader_SkipsDeleted(t *testing.T) {
	db := newTestDB(t)
	ms := &ModelService{ServiceID: "v/m", Model: "m"}
	seedModelService(t, db, ms)
	if _, err := db.Exec(`UPDATE model_services SET deleted_at = NOW(6) WHERE id = ?`, ms.ID); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}

	r := NewSQLModelServiceReader(db)
	// soft-deleted = not found = (nil, nil)
	ms, err := r.GetByModel(context.Background(), "m")
	if err != nil {
		t.Errorf("soft-deleted lookup should not error: %v", err)
	}
	if ms != nil {
		t.Errorf("expected nil for soft-deleted, got %+v", ms)
	}
	all, _ := r.List(context.Background())
	if len(all) != 0 {
		t.Errorf("List returned soft-deleted: %d", len(all))
	}
}
