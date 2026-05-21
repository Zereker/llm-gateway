-- examples/full-config/seed.sql
--
-- 示例数据：3 quota policies + 1 account + 3 model services + 3 subscriptions + 3 pricing
-- versions。endpoints + api_keys 留空（需要加密 / hash，见末尾 helper）。
--
-- 先启 gateway（启动期自跑 infra.Migrate 建表），再跑本文件 seed 业务数据。
-- 然后用 helper 工具（或自己写 Go 小脚本调 pkg/repo.EncodePayload / HashAPIKey）
-- 算 endpoints.auth 密文 + api_keys.api_key_hash 再 INSERT 即可。

-- ============================================================================
-- 1) quota_policies：三种限流档位
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
-- 3) model_services：开放给客户端选择的 model 列表
--    service_id 是业务/审计 key（vendor/model 组合）；model 是客户端 body 里写的字段
-- ============================================================================

INSERT INTO model_services (service_id, model) VALUES
('openai/gpt-4o',                       'gpt-4o'),
('anthropic/claude-3-5-sonnet',         'claude-3-5-sonnet-20241022'),
('openai/gpt-4o-via-gemini',            'gemini-1.5-pro');

-- ============================================================================
-- 4) account_model_subscriptions：demo-acme 订阅全部三个 model
-- ============================================================================

INSERT INTO account_model_subscriptions (account_id, model_service_id)
SELECT 'demo-acme', id FROM model_services;

-- ============================================================================
-- 5) endpoints：上游接入点
--
-- **auth 列 AES-256-GCM 加密**——直接 INSERT 明文 JSON 不工作。生成密文有两种方式：
--
-- (a) Go 工具脚本（推荐）：用 pkg/repo 自己的 helper
--     repo.SetDataKey(cfg.DataKey)   // 跟 gateway.yaml 里的 data_key 一致
--     auth, _ := repo.EncodePayload(repo.AuthTypeBearer, repo.BearerAuth{APIKey: "sk-..."})
--     // auth.Type = "bearer", auth.Payload = "v1:base64ofciphertext"
--
-- (b) MySQL 命令行：先用 (a) 算好密文字符串，再贴到下面的 INSERT。
--
-- routing.url 是上游 BASE URL（不含 path）；session 自己拼。
-- protocol 字段：endpoint 上游说什么协议（openai / anthropic / gemini / responses）
-- ============================================================================

-- 占位（请用 (a) 工具生成 v1:... 密文后填入）：
--   INSERT INTO endpoints (name, vendor, protocol, model, group_name, weight, enabled, auth, routing) VALUES
--   ('openai_main', 'openai', 'openai', 'gpt-4o', 'default', 100, 1,
--    'v1:base64ofEncryptedBearerAuth',
--    JSON_OBJECT('url', 'https://api.openai.com'));

-- ============================================================================
-- 6) api_keys：客户端凭证
--
-- **明文不入库**——只入 SHA-256 hex 的 hash。生成方式：
--   hash := repo.HashAPIKey(plaintext)   // 已 hex 编码的 SHA-256
--   prefix := plaintext[:12]              // 前 12 字符做 prefix（运维列表显示用）
--
-- 用 Go 脚本 / openssl / sha256sum 算出明文的 hex SHA-256 hash 填入下面。
-- ============================================================================

-- 占位：
--   INSERT INTO api_keys (account_id, api_key_hash, api_key_prefix, api_key_id,
--                          sub_account_id, group_name, quota_policy_id, enabled)
--   VALUES ('demo-acme', '<sha256-hex-of-plaintext>', 'sk-demo', 'ak_demo_alice',
--           'alice@demo-acme', 'default',
--           (SELECT id FROM quota_policies WHERE name='tier1'), 1);

-- ============================================================================
-- 7) pricing_versions：每个 (account, model_service, rule_class) 至少一条 effective_to=NULL 当前价
--
-- rule_json 是 PricingSpec 的 JSON 表示（pkg/usage/pricing.go）。
-- 这里给三个 model 都配 standard 档；BaseUnit=1K_tokens；只配 input/output 单价。
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
