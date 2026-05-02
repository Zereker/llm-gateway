package repo

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/zereker-labs/ai-gateway/pkg/domain"
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

func TestSQLModelServiceRepo_CreateAndGet(t *testing.T) {
	r := NewSQLModelServiceRepo(newTestDB(t))
	ctx := context.Background()

	snap := &domain.ModelServiceSnapshot{
		ServiceID:  "openai/gpt-4o",
		Model:      "gpt-4o",
		Group:      "default",
		Tpm:        100000,
		Rpm:        600,
		SpecDetail: json.RawMessage(`{"unit":"token"}`),
	}
	if err := r.Create(ctx, snap); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if snap.ID == 0 {
		t.Error("Create should backfill snap.ID")
	}

	got, err := r.GetByModel(ctx, "gpt-4o")
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

func TestSQLModelServiceRepo_GetNotFound(t *testing.T) {
	r := NewSQLModelServiceRepo(newTestDB(t))
	if _, err := r.GetByModel(context.Background(), "missing"); err == nil {
		t.Fatal("want not-found error")
	}
	if _, err := r.GetByModel(context.Background(), ""); err == nil {
		t.Fatal("want error for empty model")
	}
}

func TestSQLModelServiceRepo_DefaultsGroupOnEmpty(t *testing.T) {
	r := NewSQLModelServiceRepo(newTestDB(t))
	ctx := context.Background()

	snap := &domain.ModelServiceSnapshot{ServiceID: "x/y", Model: "y"} // Group ""
	if err := r.Create(ctx, snap); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, _ := r.GetByModel(ctx, "y")
	if got.Group != "default" {
		t.Errorf("Group = %q, want default", got.Group)
	}
}

func TestSQLModelServiceRepo_List(t *testing.T) {
	r := NewSQLModelServiceRepo(newTestDB(t))
	ctx := context.Background()

	for _, m := range []string{"a", "b", "c"} {
		_ = r.Create(ctx, &domain.ModelServiceSnapshot{ServiceID: "v/" + m, Model: m})
	}

	all, err := r.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("len = %d, want 3", len(all))
	}
}

func TestSQLModelServiceRepo_Update(t *testing.T) {
	r := NewSQLModelServiceRepo(newTestDB(t))
	ctx := context.Background()

	snap := &domain.ModelServiceSnapshot{ServiceID: "v/m", Model: "m", Tpm: 1}
	_ = r.Create(ctx, snap)

	snap.Tpm = 999
	snap.Rpm = 50
	if err := r.Update(ctx, snap); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, _ := r.GetByModel(ctx, "m")
	if got.Tpm != 999 || got.Rpm != 50 {
		t.Errorf("got %+v", got)
	}
	if got.UpdateTime.Equal(snap.UpdateTime) == false {
		// Update 服务端覆盖 UpdateTime；snap.UpdateTime 也应该被覆盖到 now
		// 这里只断言两边一致就够了
		t.Errorf("UpdateTime out of sync: snap=%v got=%v", snap.UpdateTime, got.UpdateTime)
	}
}

func TestSQLModelServiceRepo_UpdateMissing(t *testing.T) {
	r := NewSQLModelServiceRepo(newTestDB(t))
	err := r.Update(context.Background(), &domain.ModelServiceSnapshot{Model: "nope"})
	if err == nil {
		t.Fatal("want not-found error")
	}
}

func TestSQLModelServiceRepo_Delete(t *testing.T) {
	r := NewSQLModelServiceRepo(newTestDB(t))
	ctx := context.Background()

	_ = r.Create(ctx, &domain.ModelServiceSnapshot{ServiceID: "v/m", Model: "m"})
	if err := r.Delete(ctx, "m"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.GetByModel(ctx, "m"); err == nil {
		t.Error("expected gone after Delete")
	}
}

func TestSQLModelServiceRepo_DeleteMissing(t *testing.T) {
	r := NewSQLModelServiceRepo(newTestDB(t))
	if err := r.Delete(context.Background(), "nope"); err == nil {
		t.Fatal("want not-found error")
	}
}

func TestSQLModelServiceRepo_DuplicateModel(t *testing.T) {
	r := NewSQLModelServiceRepo(newTestDB(t))
	ctx := context.Background()
	_ = r.Create(ctx, &domain.ModelServiceSnapshot{ServiceID: "v/m", Model: "m"})

	if err := r.Create(ctx, &domain.ModelServiceSnapshot{ServiceID: "v/m2", Model: "m"}); err == nil {
		t.Fatal("want UNIQUE constraint error on duplicate model")
	}
}
