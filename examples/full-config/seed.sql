-- examples/full-config/seed.sql
--
-- 示例数据：3 quota policies + 1 tenant + 3 model services + 4 endpoints + 1 api key
--                     + 3 model subscriptions + 3 pricing versions
--
-- 先跑 pkg/infra/schema.sql 建表，再跑本文件 seed 数据。
-- gateway 启动后 `curl -H "Authorization: Bearer sk-demo-abc123def456ghi789jkl012mno345pq" \
--                     http://localhost:8080/v1/chat/completions ...` 就能跑通。
--
-- **生产警告**：本 seed 含明文 API key（仅 demo）；真生产 admin POST /admin/v1/apikeys 自动 hash。

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
-- 2) tenants
-- ============================================================================

INSERT INTO tenants (pin, name, quota_policy_id) VALUES
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
-- 4) tenant_model_subscriptions：demo-acme 订阅全部三个 model
-- ============================================================================

INSERT INTO tenant_model_subscriptions (tenant_id, model_service_id)
SELECT 'demo-acme', id FROM model_services;

-- ============================================================================
-- 5) endpoints：上游接入点
--    auth 列在生产由 admin POST 时加密（AES-GCM）；本 seed 直接写明文是为了 demo 简单。
--    生产路径：admin POST /admin/v1/endpoints 自动加密入库。
--    routing.url 是上游 BASE URL（不含 path）；session 拼 path。
--
-- **本 seed 是示意，直接 INSERT 不会工作**（auth 列 AES 加密）。生产请用：
--   curl -X POST http://localhost:8081/admin/v1/endpoints \
--     -H "X-Admin-Token: $TOKEN" \
--     -d '{"name":"openai-prod","vendor":"openai","model":"gpt-4o", "auth":{"api_key":"sk-..."}, "routing":{"url":"https://api.openai.com"}, ...}'
-- ============================================================================

-- 占位：admin 生成 auth 密文后这里看起来类似：
--   auth = 'v1:base64ofencryptedAuthConfig'

-- ============================================================================
-- 6) api_keys：客户端凭证
--    api_key_hash = SHA-256(明文 api_key) hex
--    示例明文：sk-demo-abc123def456ghi789jkl012mno345pq
--    SHA-256 = e3b0c44... （示意；真值由 admin 生成时算）
--
-- **本 seed 只示意结构**；生产用 admin POST /admin/v1/apikeys 让服务端生成 + 入库。
-- ============================================================================

-- 占位：admin 生成 hash 后这里看起来类似：
--   INSERT INTO api_keys (tenant_id, api_key_hash, api_key_prefix, api_key_id, user_id, group_name, quota_policy_id)
--     VALUES ('demo-acme', '<sha256-of-sk-demo-abc...>', 'sk-demo', 'ak_demo_alice',
--             'alice@demo-acme', 'default', (SELECT id FROM quota_policies WHERE name='tier1'));

-- ============================================================================
-- 7) pricing_versions：每个 (tenant, model_service, rule_class) 至少一条 effective_to=NULL 当前价
--
-- rule_json 是 PricingSpec 的 JSON 表示（pkg/usage/pricing.go）。
-- 这里给三个 model 都配 standard 档；BaseUnit=1K_tokens；只配 input/output 单价。
-- ============================================================================

INSERT INTO pricing_versions (tenant_id, model_service_id, rule_class, effective_from, rule_json, notes) VALUES
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
