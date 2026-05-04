package repo

import (
	"context"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"gorm.io/datatypes"
)

// seedPricingVersion 直接 INSERT 一条 pricing_versions 行；FK 要求 model_service 已存在。
func seedPricingVersion(t *testing.T, db *sqlx.DB, pv *PricingVersion) {
	t.Helper()
	if pv.TenantID == "" {
		pv.TenantID = testTenant
	}
	if pv.RuleClass == "" {
		pv.RuleClass = "standard"
	}
	if pv.EffectiveFrom.IsZero() {
		pv.EffectiveFrom = time.Now().UTC()
	}
	if len(pv.RuleJSON) == 0 {
		pv.RuleJSON = datatypes.JSON(`{"unit":"tokens_per_1m","currency":"USD","rates":{"input":5.0}}`)
	}
	res, err := db.NamedExec(
		`INSERT INTO pricing_versions
		 (tenant_id, model_service_id, rule_class, effective_from, effective_to,
		  rule_json, created_by, notes)
		 VALUES
		 (:tenant_id, :model_service_id, :rule_class, :effective_from, :effective_to,
		  :rule_json, :created_by, :notes)`,
		pv,
	)
	if err != nil {
		t.Fatalf("seed pricing_version: %v", err)
	}
	if id, err := res.LastInsertId(); err == nil {
		pv.ID = id
	}
}

func TestSQLPricingProvider_GetActive(t *testing.T) {
	db := newTestDB(t)
	// 先 seed model_service（FK 父表）
	ms := &ModelService{ServiceID: "openai/gpt-4o", Model: "gpt-4o"}
	seedModelService(t, db, ms)

	// active 版本
	now := time.Now().UTC()
	pv := &PricingVersion{
		ModelServiceID: ms.ID,
		EffectiveFrom:  now.Add(-time.Hour),
		RuleJSON:       datatypes.JSON(`{"unit":"tokens_per_1m","currency":"USD","rates":{"input":5.0}}`),
	}
	seedPricingVersion(t, db, pv)

	r := NewSQLPricingProvider(db)
	got, err := r.GetActive(context.Background(), testTenant, ms.ID, "standard", now)
	if err != nil {
		t.Fatalf("GetActive: %v", err)
	}
	if got.ID != pv.ID {
		t.Errorf("ID = %d, want %d", got.ID, pv.ID)
	}
}

func TestSQLPricingProvider_GetActiveSelectsLatestActive(t *testing.T) {
	// 多条历史 + 一条 active；保证 ORDER BY effective_from DESC LIMIT 1 取最新
	db := newTestDB(t)
	ms := &ModelService{ServiceID: "openai/gpt-4o", Model: "gpt-4o"}
	seedModelService(t, db, ms)

	now := time.Now().UTC()
	old := now.Add(-2 * time.Hour)
	mid := now.Add(-1 * time.Hour)

	// v1：已封盘
	v1End := mid
	seedPricingVersion(t, db, &PricingVersion{
		ModelServiceID: ms.ID, EffectiveFrom: old, EffectiveTo: &v1End,
		RuleJSON: datatypes.JSON(`{"v":1}`),
	})
	// v2：当前 active
	v2 := &PricingVersion{
		ModelServiceID: ms.ID, EffectiveFrom: mid,
		RuleJSON: datatypes.JSON(`{"v":2}`),
	}
	seedPricingVersion(t, db, v2)

	r := NewSQLPricingProvider(db)
	got, err := r.GetActive(context.Background(), testTenant, ms.ID, "standard", now)
	if err != nil {
		t.Fatalf("GetActive: %v", err)
	}
	if got.ID != v2.ID {
		t.Errorf("got v%d, want v2 (id=%d), got id=%d", 1, v2.ID, got.ID)
	}
}

func TestSQLPricingProvider_GetActiveNoneFails(t *testing.T) {
	// 完全没有 pricing 行 → 报错
	db := newTestDB(t)
	ms := &ModelService{ServiceID: "openai/gpt-4o", Model: "gpt-4o"}
	seedModelService(t, db, ms)

	r := NewSQLPricingProvider(db)
	_, err := r.GetActive(context.Background(), testTenant, ms.ID, "standard", time.Now().UTC())
	if err == nil {
		t.Fatal("expected error for missing active price")
	}
}

func TestSQLPricingProvider_ListHistoryDescByEffectiveFrom(t *testing.T) {
	db := newTestDB(t)
	ms := &ModelService{ServiceID: "openai/gpt-4o", Model: "gpt-4o"}
	seedModelService(t, db, ms)

	t0 := time.Now().UTC()
	for i, dt := range []time.Duration{-3 * time.Hour, -2 * time.Hour, -time.Hour} {
		seedPricingVersion(t, db, &PricingVersion{
			ModelServiceID: ms.ID, EffectiveFrom: t0.Add(dt),
			RuleJSON: datatypes.JSON(`{}`),
			Notes:    "v" + string(rune('0'+i)),
		})
	}

	r := NewSQLPricingProvider(db)
	all, err := r.ListHistory(context.Background(), testTenant, ms.ID, "standard")
	if err != nil {
		t.Fatalf("ListHistory: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("len = %d, want 3", len(all))
	}
	// 倒序：[2] 最新（-1h）, [1] (-2h), [0] (-3h)
	if !all[0].EffectiveFrom.After(all[1].EffectiveFrom) {
		t.Errorf("not sorted DESC by effective_from")
	}
}
