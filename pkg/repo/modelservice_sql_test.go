package repo

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"gorm.io/datatypes"
)

// jsonEqual 比较两个 JSON byte slice 的语义相等（不在意空格 / 字段顺序）。
// MySQL JSON 列存进去会 re-format，byte 不等但语义相等。
func jsonEqual(t *testing.T, got, want []byte) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Errorf("got not valid JSON: %v (raw: %s)", err, got)
		return false
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Errorf("want not valid JSON: %v", err)
		return false
	}
	return reflect.DeepEqual(g, w)
}

// seedModelService 用 raw SQL 把测试数据写进 db（bypass admin 写路径），
// 让 Reader 测试自包含。tenant_id 默认 "default"。
func seedModelService(t *testing.T, db *sqlx.DB, ms *ModelService) {
	t.Helper()
	if ms.TenantID == "" {
		ms.TenantID = testTenant
	}
	if ms.UpdateTime.IsZero() {
		ms.UpdateTime = time.Now().UTC()
	}
	if ms.Group == "" {
		ms.Group = "default"
	}
	res, err := db.Exec(
		`INSERT INTO model_services (tenant_id, service_id, model, update_time, spec_detail, group_name, tpm, rpm)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		ms.TenantID, ms.ServiceID, ms.Model, ms.UpdateTime, datatypesJSONOrNil(ms.SpecDetail),
		ms.Group, ms.Tpm, ms.Rpm,
	)
	if err != nil {
		t.Fatalf("seed model_service: %v", err)
	}
	if id, err := res.LastInsertId(); err == nil {
		ms.ID = id
	}
}

// datatypesJSONOrNil 避免 "" → MySQL JSON 列拒绝插入。
func datatypesJSONOrNil(j datatypes.JSON) any {
	if len(j) == 0 {
		return nil
	}
	return string(j)
}

func TestSQLModelServiceReader_GetByModel(t *testing.T) {
	db := newTestDB(t)
	seedModelService(t, db, &ModelService{
		ServiceID:  "openai/gpt-4o",
		Model:      "gpt-4o",
		Tpm:        100000,
		Rpm:        600,
		SpecDetail: datatypes.JSON(`{"unit":"token"}`),
	})

	r := NewSQLModelServiceReader(db)
	got, err := r.GetByModel(context.Background(), testTenant, "gpt-4o")
	if err != nil {
		t.Fatalf("GetByModel: %v", err)
	}
	if got.ServiceID != "openai/gpt-4o" || got.Tpm != 100000 || got.Rpm != 600 {
		t.Errorf("got %+v", got)
	}
	if !jsonEqual(t, got.SpecDetail, []byte(`{"unit":"token"}`)) {
		t.Errorf("SpecDetail = %s", got.SpecDetail)
	}
}

func TestSQLModelServiceReader_GetNotFound(t *testing.T) {
	r := NewSQLModelServiceReader(newTestDB(t))
	if _, err := r.GetByModel(context.Background(), testTenant, "missing"); err == nil {
		t.Fatal("want not-found error")
	}
	if _, err := r.GetByModel(context.Background(), testTenant, ""); err == nil {
		t.Fatal("want error for empty model")
	}
}

func TestSQLModelServiceReader_List(t *testing.T) {
	db := newTestDB(t)
	for _, m := range []string{"a", "b", "c"} {
		seedModelService(t, db, &ModelService{ServiceID: "v/" + m, Model: m})
	}

	r := NewSQLModelServiceReader(db)
	all, err := r.List(context.Background(), testTenant)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("len = %d, want 3", len(all))
	}
}
