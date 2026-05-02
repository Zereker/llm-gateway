package infra

import (
	"context"
	"testing"
)

func TestOpen_SQLiteInMemory(t *testing.T) {
	db, err := Open(DriverSQLite, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Ping 应在 Open 内部已通过
	if err := db.Ping(); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func TestOpen_UnknownDriver(t *testing.T) {
	_, err := Open(Driver("nope"), "")
	if err == nil {
		t.Fatal("want error for unknown driver")
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	db, err := Open(DriverSQLite, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate (1st): %v", err)
	}
	// 再跑一次：CREATE IF NOT EXISTS 必须不报错
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate (2nd): %v", err)
	}

	var tables []string
	if err := db.Select(&tables,
		`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`,
	); err != nil {
		t.Fatalf("query tables: %v", err)
	}
	want := map[string]bool{"model_services": false, "endpoints": false}
	for _, n := range tables {
		if _, ok := want[n]; ok {
			want[n] = true
		}
	}
	for n, ok := range want {
		if !ok {
			t.Errorf("table %q not created (got %v)", n, tables)
		}
	}
}

func TestMigrate_TableShape(t *testing.T) {
	db, err := Open(DriverSQLite, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// 简单插入 / 读回，验证字段都对得上
	_, err = db.Exec(
		`INSERT INTO model_services (service_id, model, group_name, tpm, rpm)
		 VALUES (?, ?, ?, ?, ?)`,
		"openai/gpt-4o", "gpt-4o", "default", 100000, 600,
	)
	if err != nil {
		t.Fatalf("insert model_services: %v", err)
	}

	_, err = db.Exec(
		`INSERT INTO endpoints (id, vendor, url, api_key, group_name, model, weight, rpm, tpm, rps)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"openai_main", "openai", "https://api.openai.com",
		"sk-xxx", "default", "gpt-4o", 100, 600, 100000, 0,
	)
	if err != nil {
		t.Fatalf("insert endpoints: %v", err)
	}

	var msCount, epCount int
	_ = db.Get(&msCount, `SELECT COUNT(*) FROM model_services`)
	_ = db.Get(&epCount, `SELECT COUNT(*) FROM endpoints`)
	if msCount != 1 || epCount != 1 {
		t.Errorf("counts: ms=%d ep=%d, want 1/1", msCount, epCount)
	}
}
