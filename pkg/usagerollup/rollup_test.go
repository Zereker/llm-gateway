package usagerollup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/jmoiron/sqlx"

	"github.com/zereker/llm-gateway/pkg/infra"
)

func testDB(t *testing.T) *sqlx.DB {
	t.Helper()
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		t.Skip("MYSQL_DSN not set; skipping usagerollup integration test")
	}
	db, err := infra.Open(infra.DBConfig{Driver: infra.DriverMySQL, DSN: dsn})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := infra.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := db.Exec(`TRUNCATE TABLE usage_daily`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return db
}

func evLine(acct, model, day string, in, out, total int64) string {
	return fmt.Sprintf(
		`{"schema_version":"usage.v1","event_id":"e","usage":{"input":%d,"output":%d,"total":%d,"meta":{"account_id":%q,"model":%q}},"created_at":%q}`,
		in, out, total, acct, model, day+"T10:00:00Z") + "\n"
}

func readDaily(t *testing.T, db *sqlx.DB, acct, model, day string) (int64, int64, int64, int64) {
	t.Helper()
	var r struct {
		In, Out, Total, Req int64
	}
	err := db.QueryRow(
		`SELECT input_tokens, output_tokens, total_tokens, requests
		 FROM usage_daily WHERE account_id=? AND model=? AND day=?`,
		acct, model, day).Scan(&r.In, &r.Out, &r.Total, &r.Req)
	if err != nil {
		t.Fatalf("read daily (%s/%s/%s): %v", acct, model, day, err)
	}
	return r.In, r.Out, r.Total, r.Req
}

// TestRollup_AggregatesAndIsIncremental：聚合正确 + 增量不重复计数 + 尾部半行不消费。
func TestRollup_AggregatesAndIsIncremental(t *testing.T) {
	db := testDB(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.jsonl")

	// 第一批：两条同 (acct,model,day) + 一条不同 model。
	batch1 := evLine("acct1", "gpt-4o", "2026-07-01", 10, 5, 15) +
		evLine("acct1", "gpt-4o", "2026-07-01", 20, 10, 30) +
		evLine("acct1", "claude", "2026-07-01", 100, 50, 150)
	if err := os.WriteFile(path, []byte(batch1), 0o644); err != nil {
		t.Fatal(err)
	}

	roll := New(db, path)
	res, err := roll.Run(context.Background())
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if res.Events != 3 {
		t.Fatalf("run1 events = %d, want 3", res.Events)
	}
	if in, out, total, req := readDaily(t, db, "acct1", "gpt-4o", "2026-07-01"); in != 30 || out != 15 || total != 45 || req != 2 {
		t.Errorf("gpt-4o agg = (%d,%d,%d,%d), want (30,15,45,2)", in, out, total, req)
	}

	// 再 Run 一次（无新数据）——不重复计数。
	res, err = roll.Run(context.Background())
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if res.Events != 0 {
		t.Errorf("run2 events = %d, want 0（增量不应重复）", res.Events)
	}
	if _, _, total, req := readDaily(t, db, "acct1", "gpt-4o", "2026-07-01"); total != 45 || req != 2 {
		t.Errorf("重复 run 后 gpt-4o total=%d req=%d, want 45/2（被重复计数了）", total, req)
	}

	// 追加一条完整行 + 一条**半行**（无换行）——只消费完整行。
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(evLine("acct1", "gpt-4o", "2026-07-01", 1, 1, 2))
	f.WriteString(`{"partial":true,"no_newline"`) // 半行
	f.Close()

	res, err = roll.Run(context.Background())
	if err != nil {
		t.Fatalf("run 3: %v", err)
	}
	if res.Events != 1 {
		t.Errorf("run3 events = %d, want 1（半行不该被消费）", res.Events)
	}
	if _, _, total, req := readDaily(t, db, "acct1", "gpt-4o", "2026-07-01"); total != 47 || req != 3 {
		t.Errorf("run3 后 gpt-4o total=%d req=%d, want 47/3", total, req)
	}

	// 补全那半行成完整行 → 下次 Run 应消费它。
	f, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("}\n") // 闭合上面的半行（partial 事件缺 usage/meta → 被 skip）
	f.Close()
	res, err = roll.Run(context.Background())
	if err != nil {
		t.Fatalf("run 4: %v", err)
	}
	if res.Skipped != 1 {
		t.Errorf("run4 skipped = %d, want 1（补全的 partial 行缺 account/model 被 skip）", res.Skipped)
	}
}
