package policy

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func validDecision() Decision {
	return Decision{
		Action:     ActionAllow,
		Policy:     PolicyRef{ID: "policy-safe", Version: 3, Scope: Scope{Kind: ScopeAccount, ID: "a1"}},
		RuleID:     "rule-safe",
		ReasonCode: "allowed",
	}
}

func TestDecisionValidate(t *testing.T) {
	if err := validDecision().Validate(); err != nil {
		t.Fatal(err)
	}

	tests := map[string]func(*Decision){
		"action":         func(d *Decision) { d.Action = "unknown" },
		"policy id":      func(d *Decision) { d.Policy.ID = "" },
		"policy version": func(d *Decision) { d.Policy.Version = 0 },
		"policy scope":   func(d *Decision) { d.Policy.Scope.Kind = "" },
		"rule":           func(d *Decision) { d.RuleID = "" },
		"reason":         func(d *Decision) { d.ReasonCode = "" },
		"empty redact":   func(d *Decision) { d.Action = ActionRedact },
		"allow mutation": func(d *Decision) { d.Mutations = []Mutation{{ID: "m", Kind: MutationRedact, Target: "body"}} },
		"mutation id": func(d *Decision) {
			d.Action = ActionRedact
			d.Mutations = []Mutation{{Kind: MutationRedact, Target: "body"}}
		},
		"mutation kind": func(d *Decision) {
			d.Action = ActionRedact
			d.Mutations = []Mutation{{ID: "m", Target: "body"}}
		},
		"mutation target": func(d *Decision) {
			d.Action = ActionRedact
			d.Mutations = []Mutation{{ID: "m", Kind: MutationRedact}}
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			decision := validDecision()
			mutate(&decision)
			if err := decision.Validate(); err == nil {
				t.Fatal("Validate succeeded")
			}
		})
	}
}

func TestSafeAuditNeverContainsRuntimeContent(t *testing.T) {
	secret := "4111111111111111"
	decision := validDecision()
	decision.Action = ActionRedact
	decision.ReasonCode = "sensitive_value"
	decision.Mutations = []Mutation{{
		ID: "mask-card", Kind: MutationRedact, Target: "request.messages",
		Replacement: []byte(secret),
	}}

	audit := decision.SafeAudit(StageInput)
	encoded, err := json.Marshal(audit)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("audit leaked runtime content: %s", encoded)
	}
	if len(audit.Mutations) != 1 || audit.Mutations[0].ID != "mask-card" || audit.Stage != StageInput {
		t.Fatalf("audit = %+v", audit)
	}

	decisionJSON, err := json.Marshal(decision)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(decisionJSON), secret) || strings.Contains(string(decisionJSON), "Mutations") {
		t.Fatalf("decision JSON leaked runtime-only fields: %s", decisionJSON)
	}

	inputJSON, err := json.Marshal(EvaluationInput{
		Stage: StageInput, Content: Content{MediaType: "application/json", Bytes: []byte(secret)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(inputJSON), secret) {
		t.Fatalf("evaluation input leaked content: %s", inputJSON)
	}
}

func TestSelectBindingPrecedenceAndVersions(t *testing.T) {
	subject := Subject{AccountID: "a1", ProjectID: "p1", APIKeyID: "key1"}
	bindings := []Binding{
		{Enabled: true, Policy: PolicyRef{ID: "global", Version: 9, Scope: Scope{Kind: ScopeGlobal}}},
		{Enabled: true, Policy: PolicyRef{ID: "account", Version: 4, Scope: Scope{Kind: ScopeAccount, ID: "a1"}}},
		{Enabled: true, Policy: PolicyRef{ID: "project", Version: 2, Scope: Scope{Kind: ScopeProject, ID: "p1"}}},
		{Enabled: true, Policy: PolicyRef{ID: "key", Version: 1, Scope: Scope{Kind: ScopeAPIKey, ID: "key1"}}},
		{Enabled: true, Policy: PolicyRef{ID: "key", Version: 3, Scope: Scope{Kind: ScopeAPIKey, ID: "key1"}}},
		{Enabled: false, Policy: PolicyRef{ID: "disabled", Version: math.MaxUint64, Scope: Scope{Kind: ScopeAPIKey, ID: "key1"}}},
		{Enabled: true, Policy: PolicyRef{ID: "other", Version: 99, Scope: Scope{Kind: ScopeAPIKey, ID: "other"}}},
	}

	selected := SelectBinding(bindings, subject)
	if selected == nil || selected.ID != "key" || selected.Version != 3 {
		t.Fatalf("selected = %+v", selected)
	}
	if selected := SelectBinding(bindings, Subject{AccountID: "a1"}); selected == nil || selected.ID != "account" {
		t.Fatalf("account selected = %+v", selected)
	}
	if selected := SelectBinding(bindings, Subject{}); selected == nil || selected.ID != "global" {
		t.Fatalf("global selected = %+v", selected)
	}
	if selected := SelectBinding([]Binding{{Enabled: true, Policy: PolicyRef{Scope: Scope{Kind: "unknown"}}}}, subject); selected != nil {
		t.Fatalf("unknown scope selected = %+v", selected)
	}
}

func TestPolicyRefValidate(t *testing.T) {
	tests := map[string]PolicyRef{
		"missing ID":      {Version: 1, Scope: Scope{Kind: ScopeGlobal}},
		"missing version": {ID: "p", Scope: Scope{Kind: ScopeGlobal}},
		"global with ID":  {ID: "p", Version: 1, Scope: Scope{Kind: ScopeGlobal, ID: "wrong"}},
		"account no ID":   {ID: "p", Version: 1, Scope: Scope{Kind: ScopeAccount}},
		"project no ID":   {ID: "p", Version: 1, Scope: Scope{Kind: ScopeProject}},
		"API key no ID":   {ID: "p", Version: 1, Scope: Scope{Kind: ScopeAPIKey}},
		"unknown scope":   {ID: "p", Version: 1, Scope: Scope{Kind: "unknown"}},
	}
	for name, ref := range tests {
		t.Run(name, func(t *testing.T) {
			if err := ref.Validate(); err == nil {
				t.Fatal("Validate succeeded")
			}
		})
	}
	if err := (PolicyRef{ID: "p", Version: 1, Scope: Scope{Kind: ScopeAPIKey, ID: "key"}}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDefinitionValidate(t *testing.T) {
	valid := Definition{
		Ref:  PolicyRef{ID: "p", Version: 1, Scope: Scope{Kind: ScopeGlobal}},
		Name: "default", InputEnabled: true, OutputMode: OutputStrictBuffered,
		MaxBufferBytes: 1024,
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Definition){
		"name":             func(d *Definition) { d.Name = "" },
		"mode":             func(d *Definition) { d.OutputMode = "unknown" },
		"buffer":           func(d *Definition) { d.MaxBufferBytes = -1 },
		"buffer too large": func(d *Definition) { d.MaxBufferBytes = MaxBufferBytes + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			definition := valid
			mutate(&definition)
			if err := definition.Validate(); err == nil {
				t.Fatal("Validate succeeded")
			}
		})
	}
}
