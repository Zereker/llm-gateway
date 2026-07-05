-- examples/full-config/seed.sql
--
-- Sample data: 3 quota policies + 1 account + 3 model services + 3 subscriptions + 3 pricing
-- versions. endpoints + api_keys are left empty (they need encryption / hashing; see the helper notes at the end).
--
-- Start the gateway first (it runs infra.Migrate to create tables at startup), then run this file to seed business data.
-- Then use the helper tool (or write a small Go script calling pkg/repo.EncodePayload / HashAPIKey)
-- to compute endpoints.auth ciphertext + api_keys.api_key_hash before INSERTing them.

-- ============================================================================
-- 1) quota_policies: three rate-limit tiers
-- ============================================================================

INSERT INTO quota_policies (name, description, rule_json) VALUES
('free', 'Free tier — 60 RPM, 100K TPM',
 JSON_OBJECT(
   'default', JSON_OBJECT('rpm', 60, 'tpm', 100000),
   'per_model', JSON_OBJECT(
     'gpt-4o',  JSON_OBJECT('rpm', 10, 'tpm', 30000),
     'claude-3-5-sonnet-20241022', JSON_OBJECT('rpm', 5, 'tpm', 20000)
   )
 )),
('tier1', 'Tier-1 — 600 RPM, 1M TPM',
 JSON_OBJECT(
   'default', JSON_OBJECT('rpm', 600, 'tpm', 1000000)
 )),
('enterprise', 'Enterprise — unlimited',
 JSON_OBJECT('default', JSON_OBJECT()));

-- ============================================================================
-- 2) accounts
-- ============================================================================

INSERT INTO accounts (pin, name, quota_policy_id) VALUES
('demo-acme', 'ACME Corp (demo)', (SELECT id FROM quota_policies WHERE name='tier1'));

-- ============================================================================
-- 3) model_services: the list of models exposed for clients to choose from
--    service_id is a business/audit key (vendor/model combination); model is the field clients put in the request body
-- ============================================================================

INSERT INTO model_services (service_id, model) VALUES
('openai/gpt-4o',                       'gpt-4o'),
('anthropic/claude-3-5-sonnet',         'claude-3-5-sonnet-20241022'),
('openai/gpt-4o-via-gemini',            'gemini-1.5-pro');

-- ============================================================================
-- 4) account_model_subscriptions: demo-acme subscribes to all three models
-- ============================================================================

INSERT INTO account_model_subscriptions (account_id, model_service_id)
SELECT 'demo-acme', id FROM model_services;

-- ============================================================================
-- 5) endpoints: upstream access points
--
-- **The auth column is AES-256-GCM encrypted** — inserting plaintext JSON directly will not work.
-- There are two ways to generate the ciphertext:
--
-- (a) Go tooling script (recommended): use pkg/repo's own helper
--     repo.SetDataKey(cfg.DataKey)   // must match the data_key in gateway.yaml
--     auth, _ := repo.EncodePayload(repo.AuthTypeBearer, repo.BearerAuth{APIKey: "sk-..."})
--     // auth.Type = "bearer", auth.Payload = "v1:base64ofciphertext"
--
-- (b) MySQL CLI: first compute the ciphertext string using (a), then paste it into the INSERT below.
--
-- routing.url is the upstream BASE URL (no path); the session appends the path itself.
-- protocol field: which protocol the endpoint's upstream speaks (openai / anthropic / gemini / responses)
-- ============================================================================

-- Placeholder (generate the v1:... ciphertext with tool (a) and fill it in):
--   INSERT INTO endpoints (name, vendor, protocol, model, group_name, weight, enabled, auth, routing) VALUES
--   ('openai_main', 'openai', 'openai', 'gpt-4o', 'default', 100, 1,
--    'v1:base64ofEncryptedBearerAuth',
--    JSON_OBJECT('url', 'https://api.openai.com'));

-- ============================================================================
-- 6) api_keys: client credentials
--
-- **Plaintext is never stored** — only the SHA-256 hex hash is stored. To generate it:
--   hash := repo.HashAPIKey(plaintext)   // already hex-encoded SHA-256
--   prefix := plaintext[:12]              // first 12 characters as the prefix (used for display in ops listings)
--
-- Use a Go script / openssl / sha256sum to compute the plaintext's hex SHA-256 hash and fill it in below.
-- ============================================================================

-- Placeholder:
--   INSERT INTO api_keys (account_id, api_key_hash, api_key_prefix, api_key_id,
--                          sub_account_id, group_name, quota_policy_id, enabled)
--   VALUES ('demo-acme', '<sha256-hex-of-plaintext>', 'sk-demo', 'ak_demo_alice',
--           'alice@demo-acme', 'default',
--           (SELECT id FROM quota_policies WHERE name='tier1'), 1);

-- ============================================================================
-- 7) pricing_versions: each (account, model_service, rule_class) needs at least one current price row with effective_to=NULL
--
-- rule_json is the JSON representation of PricingSpec (pkg/usage/pricing.go).
-- Here all three models are configured with the standard tier; BaseUnit=1K_tokens; only input/output unit prices are set.
-- ============================================================================

INSERT INTO pricing_versions (account_id, model_service_id, rule_class, effective_from, rule_json, notes) VALUES
('demo-acme',
 (SELECT id FROM model_services WHERE service_id='openai/gpt-4o'),
 'standard',
 '2025-01-01 00:00:00',
 JSON_OBJECT(
   'BaseUnit', '1K_tokens',
   'Rates', JSON_OBJECT('Input', 0.0025, 'Output', 0.01),
   'ModelRatio', 1.0
 ),
 'OpenAI gpt-4o public price 2025-01'),
('demo-acme',
 (SELECT id FROM model_services WHERE service_id='anthropic/claude-3-5-sonnet'),
 'standard',
 '2025-01-01 00:00:00',
 JSON_OBJECT(
   'BaseUnit', '1K_tokens',
   'Rates', JSON_OBJECT('Input', 0.003, 'Output', 0.015),
   'ModelRatio', 1.0
 ),
 'Anthropic claude-3-5-sonnet public price'),
('demo-acme',
 (SELECT id FROM model_services WHERE service_id='openai/gpt-4o-via-gemini'),
 'standard',
 '2025-01-01 00:00:00',
 JSON_OBJECT(
   'BaseUnit', '1K_tokens',
   'Rates', JSON_OBJECT('Input', 0.00125, 'Output', 0.005),
   'ModelRatio', 1.0
 ),
 'Gemini 1.5 Pro via openai_gemini translator');
