package cdc

import (
	"strings"
	"testing"
)

func TestParseEvent_FlatShape(t *testing.T) {
	// Debezium application.properties 关 schemas.enable 后的 flat envelope
	raw := []byte(`{
		"op": "c",
		"ts_ms": 1700000000000,
		"source": {"db":"llm_gateway","table":"model_services","ts_ms":1700000000000},
		"after":  {"id":1,"service_id":"openai/gpt-4o","model":"gpt-4o"}
	}`)
	e, err := ParseEvent(raw)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if e.Op != OpCreate {
		t.Errorf("op=%q", e.Op)
	}
	if e.Source.Table != "model_services" {
		t.Errorf("source.table=%q", e.Source.Table)
	}
	if e.IsDelete() {
		t.Error("not delete")
	}
	row := e.PrimaryRow()
	if !strings.Contains(string(row), `"model":"gpt-4o"`) {
		t.Errorf("primary row missing model: %s", row)
	}
}

func TestParseEvent_NestedShape(t *testing.T) {
	// Debezium 默认带 schema 的 nested envelope
	raw := []byte(`{
		"schema": {"type":"struct"},
		"payload": {
			"op":"u",
			"ts_ms":1700000000000,
			"source": {"db":"llm_gateway","table":"endpoints"},
			"before": {"id":1},
			"after":  {"id":1,"name":"updated"}
		}
	}`)
	e, err := ParseEvent(raw)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if e.Op != OpUpdate {
		t.Errorf("op=%q", e.Op)
	}
	if e.Source.Table != "endpoints" {
		t.Errorf("source.table=%q", e.Source.Table)
	}
}

func TestParseEvent_Delete_PrimaryRowFromBefore(t *testing.T) {
	raw := []byte(`{
		"op":"d","ts_ms":1,"source":{"db":"x","table":"y"},
		"before": {"id":99,"model":"deleted-model"},
		"after":  null
	}`)
	e, err := ParseEvent(raw)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !e.IsDelete() {
		t.Error("expected delete")
	}
	row := e.PrimaryRow()
	if !strings.Contains(string(row), `"model":"deleted-model"`) {
		t.Errorf("delete primary should be 'before': %s", row)
	}
}

func TestParseEvent_MissingOp_Error(t *testing.T) {
	raw := []byte(`{"source":{"db":"x"},"after":{}}`)
	if _, err := ParseEvent(raw); err == nil {
		t.Fatal("expected error for missing op")
	}
}
