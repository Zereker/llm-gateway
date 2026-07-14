-- v0.3 schema: MySQL 8.0+ dialect.
-- All DDL uses IF NOT EXISTS to keep Migrate idempotent, safe to re-run.
-- charset / collation unified to utf8mb4 / utf8mb4_unicode_ci.
--
-- **Architecture positioning**: open-platform SaaS. Dozens of upstream models, hundreds to thousands
-- of primary accounts (pin / billing subjects) subscribing based on permissions.
--
-- **Core relationships**:
--  accounts (pin)              -- primary account / billing subject; has a quota_policy attached
--      | 1:N
--   api_keys                   -- credentials; can independently attach a quota_policy
--  account_model_subscriptions -- primary account x model_services; determines visibility
--   pricing_versions           -- primary account x model_services x rule_class x time; determines unit price
--
--   model_services             -- global model catalog (no account_id)
--   endpoints                  -- global upstream connection points (no account_id; BYOK is future v0.x+)
--   routing_cost_profiles      -- immutable routing-only operator-cost snapshots
--   quota_policies             -- rate-limit policies (referenced by accounts and api_keys, N:M shared)
--
-- **Design highlights**:
--   - id is always BIGINT UNSIGNED AUTO_INCREMENT; unique business keys use composite UNIQUE
--   - the accounts table stores primary account pin / billing subject metadata
--   - accounts uses pin (VARCHAR PK) as the business key; the account_id column in other tables
--     is a FK -> accounts(pin)
--   - string fields are VARCHAR with explicit length (not TEXT, so they can be indexed)
--   - JSON blob columns use MySQL 8.0's native JSON type
--   - all timestamps are TIMESTAMP(6) with microsecond precision, aligned with Go time.Time
--   - the standard trio: created_at + updated_at (ON UPDATE) + deleted_at (NULL) soft delete
--
-- **Soft-delete limitation**: a row with deleted_at = NULL still occupies its UNIQUE key,
-- so after a soft delete the same key cannot be reused directly (a hard delete is required).
-- v0.x accepts this limitation.
--
-- **Schema load order matters**: FK dependencies determine the CREATE order:
--   quota_policies -> accounts -> model_services -> routing_cost_profiles -> endpoints
--                  -> account_model_subscriptions -> api_keys -> pricing_versions
-- DROP order is the reverse (child tables must be dropped before their parent tables).

-- =====================================================================
-- quota_policies: rate-limit policy library (referenced N:M by primary accounts / api_keys)
--
-- rule_json shape (interpreted by M6 RateLimit; the gateway itself does not parse it):
--   {
--     "default":   {"rpm":60, "tpm":100000, "rps":null, "concurrent_requests":null},
--     "per_model": {"gpt-4o":{"rpm":10, "tpm":30000}, "gpt-4o-mini":{"rpm":100}}
--   }
-- M6 selection strategy: check per_model[currentModel] first, fall back to default,
-- and if neither exists this layer applies no limit.
--
-- Mutable (unlike pricing_versions, which is append-only; adjusting quota does not
-- affect historical billing).
-- =====================================================================
CREATE TABLE IF NOT EXISTS quota_policies (
    id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    name        VARCHAR(64)  NOT NULL,                    -- "free" | "tier1" | "tier2" | "premium" | "enterprise_acme"
    description VARCHAR(512) NOT NULL DEFAULT '',
    rule_json   JSON         NOT NULL,
    enabled     TINYINT(1)   NOT NULL DEFAULT 1,
    created_at  TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at  TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    deleted_at  TIMESTAMP(6) NULL DEFAULT NULL,
    UNIQUE KEY uk_name (name),
    INDEX idx_deleted_at (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- =====================================================================
-- accounts: primary account pin / billing subject metadata (quota_policy reference attached up front)
--
-- An "account" here is not a sub-account, but a primary account / billing subject.
-- pin is the PK directly: the business key is the identity key, no BIGINT indirection layer
-- is introduced. The account_id VARCHAR(64) column in other tables is this same pin
-- (FK -> accounts.pin).
--
-- quota_policy_id NULL = no limit at the primary-account layer (M6 skips the primary-account
-- layer check).
-- =====================================================================
CREATE TABLE IF NOT EXISTS accounts (
    pin             VARCHAR(64)  NOT NULL PRIMARY KEY,
    name            VARCHAR(128) NOT NULL,                -- display name ("Ads business line" / "Search business line" / "Default primary account")
    enabled         TINYINT(1)   NOT NULL DEFAULT 1,
    quota_policy_id BIGINT UNSIGNED NULL,                 -- NULL = no limit at the primary-account layer
    created_at      TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at      TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    deleted_at      TIMESTAMP(6) NULL DEFAULT NULL,
    INDEX idx_quota_policy (quota_policy_id),
    INDEX idx_deleted_at (deleted_at),
    CONSTRAINT fk_account_quota FOREIGN KEY (quota_policy_id) REFERENCES quota_policies(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- seed default account (used by the gateway at startup if a default account exists)
INSERT IGNORE INTO accounts (pin, name) VALUES ('default', 'Default Account');

-- =====================================================================
-- model_services: the "global model catalog" queried by the M5 ModelService middleware
--
-- **v0.3 changes**:
--   - removed account_id (the catalog is globally shared; visibility is decided by
--     account_model_subscriptions)
--   - removed group_name (M5 does not look up group; group is an endpoint-level concept)
--   - removed spec_detail (no consumer; if per-model spec is really needed later, use a
--     typed struct)
--   - UNIQUE changed to (service_id), (model) (no per-account partitioning)
-- =====================================================================
CREATE TABLE IF NOT EXISTS model_services (
    id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    service_id   VARCHAR(191) NOT NULL,                   -- "openai/gpt-4o" / "ark/deepseek-v3-2"; business/audit grouping
    model        VARCHAR(191) NOT NULL,                   -- the value of the model field in the client's request body (canonical name)

    created_at   TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at   TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    deleted_at   TIMESTAMP(6) NULL DEFAULT NULL,

    UNIQUE KEY uk_service_id (service_id),
    UNIQUE KEY uk_model (model),
    INDEX idx_deleted_at (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- =====================================================================
-- model_aliases: model alias -> canonical model redirect
--
-- Clients request an alias (e.g. "fast" / a vendor-neutral name), and M5 resolves it to
-- the canonical model_services.model (e.g. "gpt-4o-mini"); everything downstream
-- (subscription / routing / metering) then operates on the canonical name. The alias
-- is transparent to downstream consumers.
-- No FK: this is a lightweight redirect; if the canonical model is missing, resolution
-- simply misses and M5 returns 404, rather than being blocked by referential integrity.
-- =====================================================================
CREATE TABLE IF NOT EXISTS model_aliases (
    alias      VARCHAR(191) NOT NULL PRIMARY KEY,     -- the name requested by the client (unique)
    model      VARCHAR(191) NOT NULL,                 -- canonical model_services.model
    enabled    TINYINT(1)   NOT NULL DEFAULT 1,
    created_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    deleted_at TIMESTAMP(6) NULL DEFAULT NULL,
    INDEX idx_model (model),
    INDEX idx_deleted_at (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- =====================================================================
-- routing_policies: immutable versions of virtual-model routing rules
--
-- One enabled version is selected by scope precedence (account, then global).
-- rule_json contains typed candidates and deterministic constraints. Updating
-- or rolling back publishes a new monotonically increasing version.
-- =====================================================================
CREATE TABLE IF NOT EXISTS routing_policies (
    id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    policy_id     VARCHAR(64)  NOT NULL,
    version       BIGINT UNSIGNED NOT NULL,
    scope_kind    VARCHAR(16)  NOT NULL,
    scope_id      VARCHAR(64)  NOT NULL DEFAULT '',
    virtual_model VARCHAR(191) NOT NULL,
    rule_json     JSON         NOT NULL,
    enabled       TINYINT(1)   NOT NULL DEFAULT 1,
    created_by    VARCHAR(128) NOT NULL DEFAULT '',
    created_at    TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    deleted_at    TIMESTAMP(6) NULL DEFAULT NULL,

    UNIQUE KEY uk_routing_policy_version (policy_id, version),
    UNIQUE KEY uk_routing_scope_version (scope_kind, scope_id, virtual_model, version),
    INDEX idx_routing_resolve (virtual_model, scope_kind, scope_id, enabled, version),
    INDEX idx_deleted_at (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- =====================================================================
-- routing_cost_profiles: compact, immutable operator-cost snapshots
--
-- These values exist only for routing optimization. They are deliberately
-- separate from account pricing, discounts, settlement, and invoices.
-- =====================================================================
CREATE TABLE IF NOT EXISTS routing_cost_profiles (
    id                                  BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    profile_id                          VARCHAR(64) NOT NULL,
    version                             BIGINT UNSIGNED NOT NULL,
    model_service_id                    BIGINT UNSIGNED NOT NULL,
    input_microusd_per_million_tokens   BIGINT UNSIGNED NOT NULL,
    output_microusd_per_million_tokens  BIGINT UNSIGNED NOT NULL,
    enabled                             TINYINT(1) NOT NULL DEFAULT 1,
    created_by                          VARCHAR(128) NOT NULL DEFAULT '',
    created_at                          TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    deleted_at                          TIMESTAMP(6) NULL DEFAULT NULL,

    UNIQUE KEY uk_routing_cost_version (profile_id, version),
    UNIQUE KEY uk_routing_cost_model_version (model_service_id, version),
    INDEX idx_routing_cost_active (model_service_id, enabled, version),
    INDEX idx_deleted_at (deleted_at),
    CONSTRAINT fk_routing_cost_model FOREIGN KEY (model_service_id) REFERENCES model_services(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- =====================================================================
-- account_model_subscriptions: N:M visibility table between primary accounts and model_services
--
-- A primary account that requests a model it hasn't subscribed to -> M5 returns
-- 403 "model not subscribed".
-- =====================================================================
CREATE TABLE IF NOT EXISTS account_model_subscriptions (
    id                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
   account_id         VARCHAR(64)  NOT NULL,              -- primary account pin -> accounts.pin
    model_service_id  BIGINT UNSIGNED NOT NULL,           -- -> model_services.id
    enabled           TINYINT(1)   NOT NULL DEFAULT 1,    -- soft-disable (keeps the subscription record but disables it)

    created_at        TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at        TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    deleted_at        TIMESTAMP(6) NULL DEFAULT NULL,

    UNIQUE KEY uk_account_model (account_id, model_service_id),
    INDEX idx_account (account_id),
    INDEX idx_deleted_at (deleted_at),
    CONSTRAINT fk_subscription_account FOREIGN KEY (account_id) REFERENCES accounts(pin),
    CONSTRAINT fk_subscription_model FOREIGN KEY (model_service_id) REFERENCES model_services(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- =====================================================================
-- endpoints: the "global upstream access points" that the M7 Schedule middleware routes to
--
-- **v0.3 changes**: removed account_id (the platform operator manages upstreams uniformly;
-- when BYOK etc. are actually needed, add a nullable account_id column). UNIQUE changed to
-- (name), globally unique.
--
-- Core columns hold only what the scheduling/routing hot path needs; everything
-- vendor-specific goes into typed JSON.
-- =====================================================================
CREATE TABLE IF NOT EXISTS endpoints (
    id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    name         VARCHAR(128) NOT NULL,                    -- business name (shown in ops / monitoring)
    vendor       VARCHAR(32)  NOT NULL,                    -- openai|anthropic|gemini|bedrock|vertex|azure-openai|ark
    protocol     VARCHAR(32)  NOT NULL,                    -- openai|anthropic|gemini|responses|... which protocol the upstream speaks (added in v0.6; an endpoint-level property, no longer vendor-level)
    model        VARCHAR(191) NOT NULL,                    -- used by M7 for routing
    group_name   VARCHAR(64)  NOT NULL DEFAULT 'default',  -- endpoint pool grouping
    weight       INT UNSIGNED NOT NULL DEFAULT 100,
    enabled      TINYINT(1)   NOT NULL DEFAULT 1,

    -- typed JSON: schema determined by the Go struct (internal/repo/{auth,routing,quota}_config.go)
    -- the auth column stores AES-GCM ciphertext ("v1:" + base64), which is not valid JSON,
    -- hence VARCHAR; the other columns remain JSON so MySQL performs shape validation.
    auth         VARCHAR(2048) NOT NULL,                   -- AuthConfig ciphertext (encrypt/decrypt via Scanner/Valuer on the Go side)
    routing      JSON          NOT NULL,                   -- RoutingConfig (url/region/project/...)
    quota        JSON          DEFAULT NULL,                -- upstream hard-constraint quota (distinct from quota_policies; this is a vendor-side limit)
    capabilities JSON          DEFAULT NULL,               -- EndpointCapabilities: modalities + self_hosted + prefix_cache_enabled, etc.
    quirks       JSON          DEFAULT NULL,               -- endpoint-level body / header tweak DSL (internal/protocol/quirks)
    extra        JSON          DEFAULT NULL,

    created_at   TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at   TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    deleted_at   TIMESTAMP(6) NULL DEFAULT NULL,

    UNIQUE KEY uk_name (name),
    INDEX idx_model_group (model, group_name),
    INDEX idx_deleted_at (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Note: this file only contains CREATE TABLE IF NOT EXISTS statements. Adding columns to
-- existing tables goes through ensureColumn in infra.Migrate (which ALTERs after checking
-- information_schema) -- MySQL does **not** support `ADD COLUMN IF NOT EXISTS` (that's
-- MariaDB syntax); writing it here would cause a syntax error on mysql:8.0 and prevent
-- the gateway from starting.

-- =====================================================================
-- api_keys: credential lookup table for the M2 Auth middleware
--
-- **v0.3 changes**: added the quota_policy_id column (API-key-level rate limiting; stacks
-- with the primary-account-level accounts.quota_policy). account_id FK -> accounts.pin.
-- =====================================================================
CREATE TABLE IF NOT EXISTS api_keys (
    id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
   account_id       VARCHAR(64)  NOT NULL DEFAULT 'default', -- primary account pin / billing subject
    api_key_hash    CHAR(64)     NOT NULL,                 -- hex of SHA-256(sk-XXX)
    api_key_prefix  VARCHAR(16)  NOT NULL,                 -- "sk-abc1de2f" for display
    api_key_id      VARCHAR(64)  NOT NULL,                 -- stable audit ID (e.g. ak_alice_xxxxx)
    name            VARCHAR(64)  NOT NULL DEFAULT '',      -- human-readable key label: prod/dev/ci-bot
    sub_account_id         VARCHAR(64)  NOT NULL,                 -- sub-account / operator
    group_name      VARCHAR(64)  NOT NULL DEFAULT 'default',
    external_user   TINYINT(1)   NOT NULL DEFAULT 0,
    enabled         TINYINT(1)   NOT NULL DEFAULT 1,
    expires_at      TIMESTAMP(6) NULL DEFAULT NULL,
    last_used_at    TIMESTAMP(6) NULL DEFAULT NULL,
    revoked_at      TIMESTAMP(6) NULL DEFAULT NULL,
    quota_policy_id BIGINT UNSIGNED NULL,                  -- NULL = no limit at the key layer; non-NULL stacks with the primary-account quota

    created_at      TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at      TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    deleted_at      TIMESTAMP(6) NULL DEFAULT NULL,

    UNIQUE KEY uk_api_key_hash (api_key_hash),
    UNIQUE KEY uk_account_api_key_id (account_id, api_key_id),
    INDEX idx_account_sub_account_id (account_id, sub_account_id),
    INDEX idx_expires_at (expires_at),
    INDEX idx_quota_policy (quota_policy_id),
    INDEX idx_deleted_at (deleted_at),
    CONSTRAINT fk_apikey_account FOREIGN KEY (account_id) REFERENCES accounts(pin),
    CONSTRAINT fk_apikey_quota FOREIGN KEY (quota_policy_id) REFERENCES quota_policies(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- =====================================================================
-- pricing_versions: the M5 ModelService middleware takes a "price snapshot" on the request path
--
-- **append-only**: once rule_json is published it is NEVER UPDATEd. Changing a price =
-- one transaction that closes out the old row and inserts a new one.
-- See the previous version's docstring for details; this version only adds the FK -> accounts.pin.
-- =====================================================================
CREATE TABLE IF NOT EXISTS pricing_versions (
    id               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
   account_id        VARCHAR(64)  NOT NULL DEFAULT 'default',
    model_service_id BIGINT UNSIGNED NOT NULL,
    rule_class       VARCHAR(64)  NOT NULL DEFAULT 'standard',

    effective_from   TIMESTAMP(6) NOT NULL,
    effective_to     TIMESTAMP(6) NULL DEFAULT NULL,

    rule_json        JSON         NOT NULL,

    created_at       TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    created_by       VARCHAR(128) NOT NULL DEFAULT '',
    notes            VARCHAR(512) NOT NULL DEFAULT '',

    INDEX idx_active_lookup (account_id, model_service_id, rule_class, effective_from),
    INDEX idx_effective_to (effective_to),
    CONSTRAINT fk_pricing_account FOREIGN KEY (account_id) REFERENCES accounts(pin),
    CONSTRAINT fk_pricing_model_service FOREIGN KEY (model_service_id) REFERENCES model_services(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Usage/metering deliberately has no aggregate table: the gateway is only responsible for
-- emitting usage events through the outbox (file source-of-truth + Kafka broadcast); the
-- downstream metering/billing system consumes them. The control plane does not aggregate
-- usage, to keep the independently complex "billing" domain out of the data plane / control plane.

-- =====================================================================
-- audit_log: audit trail for control-plane write operations (who - when - on what - result)
--
-- **Deliberately does not record the request body**: an endpoint-creation body carries an
-- upstream secret, and issuing a key involves credentials -- only actor / method / path /
-- status_code are recorded, so the audit trail is traceable without ever persisting
-- ciphertext/secrets.
-- The data plane never reads this table; it is control-plane only.
-- =====================================================================
CREATE TABLE IF NOT EXISTS audit_log (
    id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    actor       VARCHAR(128) NOT NULL DEFAULT '',    -- the token's name (or role as fallback)
    role        VARCHAR(32)  NOT NULL DEFAULT '',
    method      VARCHAR(8)   NOT NULL,
    path        VARCHAR(512) NOT NULL,
    status_code INT          NOT NULL,
    created_at  TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    INDEX idx_created_at (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
