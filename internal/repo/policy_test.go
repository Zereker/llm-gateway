package repo

import (
	"context"
	"testing"
	"time"

	"github.com/zereker/llm-gateway/internal/policy"
)

func TestSQLPolicyDefinitionReaderScopePrecedence(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO accounts (pin, name) VALUES ('a1', 'A1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO api_keys (account_id, api_key_hash, api_key_prefix, api_key_id, sub_account_id)
		 VALUES ('a1', REPEAT('a',64), 'sk-test', 'key1', 'u1')`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"global", "account", "key"} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO policy_definitions (policy_id, version, name, input_enabled, output_mode, max_buffer_bytes)
			 VALUES (?, 1, ?, 1, 'strict_buffered', 2048)`, id, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, binding := range []struct{ kind, scope, id string }{
		{"global", "", "global"}, {"account", "a1", "account"}, {"api_key", "key1", "key"},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO policy_bindings (scope_kind, scope_id, policy_id, policy_version) VALUES (?, ?, ?, 1)`,
			binding.kind, binding.scope, binding.id); err != nil {
			t.Fatal(err)
		}
	}

	reader := NewSQLPolicyDefinitionReader(db)
	for name, test := range map[string]struct {
		subject policy.Subject
		want    string
	}{
		"key":     {policy.Subject{AccountID: "a1", APIKeyID: "key1"}, "key"},
		"account": {policy.Subject{AccountID: "a1"}, "account"},
		"global":  {policy.Subject{AccountID: "other"}, "global"},
	} {
		t.Run(name, func(t *testing.T) {
			definition, err := reader.Resolve(ctx, test.subject)
			if err != nil || definition == nil || definition.Ref.ID != test.want {
				t.Fatalf("definition=%+v err=%v", definition, err)
			}
		})
	}

	cached := NewCachedPolicyDefinitionReader(reader, 10, time.Minute, nil)
	if definition, err := cached.Resolve(ctx, policy.Subject{AccountID: "a1"}); err != nil || definition.Ref.ID != "account" {
		t.Fatalf("cached definition=%+v err=%v", definition, err)
	}
	cached.EvictAll()
}
