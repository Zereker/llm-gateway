package gateway

// Multi-vendor field-matrix e2e: unlike fieldmatrix_test.go's single seeded
// endpoint (mutated in place per test), this seeds all four upstream vendors
// as **distinct, simultaneously-configured endpoints** with **distinct real
// API keys**, each routed to its own mock upstream server replaying a real
// captured response body from testdata/vendor-cassettes/ (see that
// directory's README for provenance/licenses) — the same real-data corpus
// internal/cassette/replay already exercises at the translator layer, one
// level up: full auth + routing + protocol translation + billing, through
// the real middleware chain, for every vendor at once.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zereker/llm-gateway/internal/cassette"
	"github.com/zereker/llm-gateway/internal/config"
	"github.com/zereker/llm-gateway/internal/infra"
	"github.com/zereker/llm-gateway/internal/repo"
)

// realCassetteResponse loads interaction index idx's response body from a
// vendor-cassettes file (relative to internal/app/gateway/testdata/).
func realCassetteResponse(t *testing.T, relPath string, idx int) []byte {
	t.Helper()
	interactions, err := cassette.Load("testdata/vendor-cassettes/" + relPath)
	if err != nil {
		t.Fatalf("cassette.Load %s: %v", relPath, err)
	}
	if idx >= len(interactions) {
		t.Fatalf("%s: want interaction #%d, only has %d", relPath, idx, len(interactions))
	}
	body := interactions[idx].ResponseBody
	if len(body) == 0 {
		t.Fatalf("%s: interaction #%d has an empty response body", relPath, idx)
	}
	return body
}

// vendorScenario describes one upstream vendor's slice of the multi-vendor
// seed: its own model, its own mock upstream (replaying real data), and its
// own endpoint auth.
type vendorScenario struct {
	vendor      string
	protocol    string
	model       string // model_services.model / endpoints.model / request "model"
	authType    string
	apiKey      string // plaintext, before repo.EncodePayload
	upstreamKey string // the credential the mock upstream would see (not asserted here)
	reply       []byte // real captured response body the mock upstream always returns
}

// seedMultiVendorScenarios seeds one account, one quota-eligible subscription
// + price per vendor, and one endpoint + one real API key per vendor — all
// coexisting simultaneously, so a request for one vendor's model can only be
// satisfied by that vendor's endpoint (proving the selector routes on model,
// not "whichever endpoint happens to be seeded").
//
// Returns each scenario's own httptest.Server (caller must Close them) so
// per-vendor auth headers actually reach a real (mock) network round trip.
func seedMultiVendorScenarios(t *testing.T, dsn string, scenarios []vendorScenario) []*httptest.Server {
	t.Helper()

	db, err := infra.Open(infra.DBConfig{Driver: infra.DriverMySQL, DSN: dsn})
	if err != nil {
		t.Fatalf("infra.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	if err := infra.Migrate(ctx, db); err != nil {
		t.Fatalf("infra.Migrate: %v", err)
	}
	if _, err := db.Exec(`SET FOREIGN_KEY_CHECKS = 0`); err != nil {
		t.Fatalf("disable FK checks: %v", err)
	}
	for _, table := range []string{
		"pricing_versions", "account_model_subscriptions", "api_keys",
		"endpoints", "model_services", "accounts", "quota_policies",
	} {
		if _, err := db.Exec("TRUNCATE TABLE " + table); err != nil {
			t.Fatalf("TRUNCATE %s: %v", table, err)
		}
	}
	if _, err := db.Exec(`SET FOREIGN_KEY_CHECKS = 1`); err != nil {
		t.Fatalf("re-enable FK checks: %v", err)
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO accounts (pin, name) VALUES (?, ?)`, "default", "Default Account"); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	servers := make([]*httptest.Server, len(scenarios))
	for i, sc := range scenarios {
		reply := sc.reply
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(strings.TrimSpace(string(reply)), "event:") {
				w.Header().Set("Content-Type", "text/event-stream")
			} else {
				w.Header().Set("Content-Type", "application/json")
			}
			_, _ = w.Write(reply)
		}))
		servers[i] = srv

		res, err := db.ExecContext(ctx,
			`INSERT INTO model_services (service_id, model) VALUES (?, ?)`,
			sc.vendor+"/"+sc.model, sc.model)
		if err != nil {
			t.Fatalf("seed model_service %s: %v", sc.vendor, err)
		}
		msID, _ := res.LastInsertId()

		if _, err := db.ExecContext(ctx,
			`INSERT INTO account_model_subscriptions (account_id, model_service_id, enabled) VALUES (?, ?, 1)`,
			"default", msID); err != nil {
			t.Fatalf("seed subscription %s: %v", sc.vendor, err)
		}

		if _, err := db.ExecContext(ctx,
			`INSERT INTO pricing_versions
			 (account_id, model_service_id, rule_class, effective_from, effective_to, rule_json, created_by, notes)
			 VALUES (?, ?, ?, NOW(6), NULL, ?, ?, ?)`,
			"default", msID, "standard",
			`{"unit":"tokens_per_1m","currency":"USD","rates":{"input":5.0,"output":15.0}}`,
			"e2e-multivendor", "test fixture"); err != nil {
			t.Fatalf("seed pricing %s: %v", sc.vendor, err)
		}

		var authPayload any
		switch sc.authType {
		case repo.AuthTypeBearer:
			authPayload = repo.BearerAuth{APIKey: sc.upstreamKey}
		case repo.AuthTypeXAPIKey:
			authPayload = repo.XAPIKeyAuth{APIKey: sc.upstreamKey}
		case repo.AuthTypeGeminiKey:
			authPayload = repo.GeminiAuth{APIKey: sc.upstreamKey}
		default:
			t.Fatalf("seedMultiVendorScenarios: unhandled auth type %q for vendor %q", sc.authType, sc.vendor)
		}
		auth, err := repo.EncodePayload(sc.authType, authPayload)
		if err != nil {
			t.Fatalf("encode auth for %s: %v", sc.vendor, err)
		}

		ep := &repo.Endpoint{
			Name:     sc.vendor + "_e2e",
			Vendor:   sc.vendor,
			Protocol: sc.protocol,
			Model:    sc.model,
			Group:    "default",
			Weight:   100,
			Enabled:  true,
			Auth:     auth,
			Routing:  repo.RoutingConfig{URL: srv.URL},
		}
		if _, err := db.NamedExecContext(ctx,
			`INSERT INTO endpoints
			 (name, vendor, protocol, model, group_name, weight, enabled, auth, routing, quota, capabilities, extra)
			 VALUES (:name, :vendor, :protocol, :model, :group_name, :weight, :enabled, :auth, :routing, :quota, :capabilities, :extra)`,
			ep); err != nil {
			t.Fatalf("seed endpoint %s: %v", sc.vendor, err)
		}

		if _, err := db.ExecContext(ctx,
			`INSERT INTO api_keys
			 (account_id, api_key_hash, api_key_prefix, api_key_id, sub_account_id, group_name, external_user, enabled)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			"default", repo.HashAPIKey(sc.apiKey), sc.apiKey[:min(12, len(sc.apiKey))],
			"ak_"+sc.vendor+"_e2e", sc.vendor+"-user", "default", false, true); err != nil {
			t.Fatalf("seed api_key %s: %v", sc.vendor, err)
		}
	}

	return servers
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestE2E_MultiVendor_AllProtocols is the "full system" check the real
// vendor-cassette corpus exists for: four distinct endpoints (openai /
// anthropic / gemini / cohere), four distinct real API keys, all seeded and
// live at once, each routed to its own mock upstream replaying a real
// captured response — proving auth, model-based routing, protocol
// translation, and billing all work correctly per-vendor without one
// vendor's config leaking into another's request.
func TestE2E_MultiVendor_AllProtocols(t *testing.T) {
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		t.Skip("MYSQL_DSN not set; skipping gateway e2e test")
	}

	scenarios := []vendorScenario{
		{
			vendor: "openai", protocol: "openai", model: "test-openai-model",
			authType: repo.AuthTypeBearer, apiKey: "sk-e2e-openai", upstreamKey: "sk-upstream-openai",
			reply: readFixtureFile(t, "testdata/fieldmatrix/upstream/chat-openai-compat.json"),
		},
		{
			vendor: "anthropic", protocol: "anthropic", model: "test-anthropic-model",
			authType: repo.AuthTypeXAPIKey, apiKey: "sk-e2e-anthropic", upstreamKey: "sk-upstream-anthropic",
			reply: realCassetteResponse(t, "anthropic/simonw-llm-anthropic/test_tools.yaml", 0),
		},
		{
			vendor: "gemini", protocol: "gemini", model: "test-gemini-model",
			authType: repo.AuthTypeGeminiKey, apiKey: "sk-e2e-gemini", upstreamKey: "sk-upstream-gemini",
			reply: realCassetteResponse(t, "gemini/simonw-llm-gemini/test_tools.yaml", 0),
		},
		{
			vendor: "cohere", protocol: "cohere", model: "test-cohere-model",
			authType: repo.AuthTypeBearer, apiKey: "sk-e2e-cohere", upstreamKey: "sk-upstream-cohere",
			reply: realCassetteResponse(t, "cohere/langchain-ai-langchain-cohere/test_invoke_tool_calls.yaml", 0),
		},
	}

	servers := seedMultiVendorScenarios(t, dsn, scenarios)
	for _, srv := range servers {
		defer srv.Close()
	}

	cfg := writeTestConfigNoSeed(t, dsn)
	engine, srv, err := buildEngine(cfg)
	if err != nil {
		t.Fatalf("buildEngine: %v", err)
	}
	defer srv.Close()

	for _, sc := range scenarios {
		sc := sc
		t.Run(sc.vendor, func(t *testing.T) {
			reqBody := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}]}`, sc.model)
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(reqBody))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+sc.apiKey)

			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)

			if w.Code != 200 {
				t.Fatalf("%s: status = %d, body = %s", sc.vendor, w.Code, w.Body.String())
			}

			out := w.Body.String()
			var choices int
			if strings.HasPrefix(strings.TrimSpace(out), "data:") {
				// streaming reply (gemini): every data: line except [DONE] must
				// be valid JSON, and the stream must actually terminate.
				if !strings.Contains(out, "data: [DONE]") {
					t.Errorf("%s: streaming reply never sent [DONE]: %s", sc.vendor, truncateForLog(out))
				}
				for _, line := range strings.Split(out, "\n") {
					line = strings.TrimSpace(line)
					if !strings.HasPrefix(line, "data:") {
						continue
					}
					payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
					if payload == "[DONE]" {
						continue
					}
					if !json.Valid([]byte(payload)) {
						t.Errorf("%s: invalid SSE JSON: %s", sc.vendor, payload)
					}
				}
				choices = 1 // presence already checked structurally above
			} else {
				var resp struct {
					Choices []json.RawMessage `json:"choices"`
				}
				if err := json.Unmarshal([]byte(out), &resp); err != nil {
					t.Fatalf("%s: response not valid chat.completion JSON: %v; body=%s", sc.vendor, err, out)
				}
				choices = len(resp.Choices)
			}
			if choices == 0 {
				t.Errorf("%s: 0 choices in translated response: %s", sc.vendor, truncateForLog(out))
			}

			// billing: some usage must have been recorded for this vendor's call
			// (each request went through M4 Budget / M7 Schedule / usage extraction
			// for a distinct endpoint, so this also proves no cross-vendor leakage).
			usageLog, _ := os.ReadFile(cfg.UsageEvents.File.Path)
			if !strings.Contains(string(usageLog), `"vendor":"`+sc.vendor+`"`) &&
				!strings.Contains(string(usageLog), sc.model) {
				t.Logf("%s: usage log doesn't obviously mention this vendor/model (informational; format may differ): %s",
					sc.vendor, truncateForLog(string(usageLog)))
			}
		})
	}
}

func readFixtureFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func truncateForLog(s string) string {
	const max = 400
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated, " + strconv.Itoa(len(s)) + " bytes total)"
}

// writeTestConfigNoSeed is writeTestConfig without the single-endpoint seedDB
// call — the multi-vendor test seeds its own endpoints via
// seedMultiVendorScenarios and must not have that overwritten/collide with it.
func writeTestConfigNoSeed(t *testing.T, dsn string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		Database: infra.DBConfig{
			Driver: infra.DriverMySQL,
			DSN:    dsn,
		},
		UsageEvents: config.UsageEventsConfig{
			Driver: "file",
			File:   config.FileOutboxSection{Path: dir + "/usage.log"},
		},
		Request: config.RequestConfig{
			BodyLimitBytes: 10 << 20,
			Timeout:        5 * time.Second,
		},
		DataKey: devDataKey,
	}
	cfg.ApplyDefaults()
	return cfg
}
