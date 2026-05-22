-- v0.3 schema：MySQL 8.0+ 方言。
-- 全部 DDL 用 IF NOT EXISTS 保证 Migrate 幂等，反复 Run 不报错。
-- charset / collation 统一 utf8mb4 / utf8mb4_unicode_ci。
--
-- **架构定位**：开放平台 SaaS。几十个上游模型，几百几千主账号（pin / 计费主体）按权限订阅。
--
-- **核心关系**：
--  accounts (pin)              ── 主账号 / 计费主体；挂 quota_policy
--      ↓ 1:N
--   api_keys                   ── 凭证；可独立挂 quota_policy
--  account_model_subscriptions ── 主账号 × model_services；决定可见性
--   pricing_versions           ── 主账号 × model_services × rule_class × time；决定单价
--
--   model_services             ── 全局模型 catalog（无 account_id）
--   endpoints                  ── 全局上游连接点（无 account_id；BYOK 是未来 v0.x+）
--   quota_policies             ── 限流策略（被 accounts 和 api_keys 引用，N:M 共享）
--
-- **设计要点**：
--   - id 一律 BIGINT UNSIGNED AUTO_INCREMENT；唯一业务键复合 UNIQUE
--   - accounts 表存主账号 pin / 计费主体元信息
--   - accounts 用 pin (VARCHAR PK) 作为业务键；其它表 account_id 字段 FK → accounts(pin)
--   - 字符串字段 VARCHAR + 明确长度（不用 TEXT，方便走索引）
--   - JSON blob 列用 MySQL 8.0 原生 JSON 类型
--   - 时间戳全部 TIMESTAMP(6) 微秒精度，跟 Go time.Time 对齐
--   - 三件套：created_at + updated_at(ON UPDATE) + deleted_at(NULL) 软删除
--
-- **soft delete 限制**：deleted_at = NULL 的行仍占用 UNIQUE 键，软删后不能直接复用同名键
-- （需 hard-delete）。v0.x 接受此限制。
--
-- **schema 加载顺序敏感**：FK 依赖决定 CREATE 顺序：
--   quota_policies → accounts → model_services → endpoints
--                  → account_model_subscriptions → api_keys → pricing_versions
-- DROP 时反向（DROP 父表前 child 必须先走）。

-- =====================================================================
-- quota_policies：限流策略库（N:M 被主账号 / api_keys 引用）
--
-- rule_json shape（M6 RateLimit 解释；gateway 不解析）：
--   {
--     "default":   {"rpm":60, "tpm":100000, "rps":null, "concurrent_requests":null},
--     "per_model": {"gpt-4o":{"rpm":10, "tpm":30000}, "gpt-4o-mini":{"rpm":100}}
--   }
-- M6 选择策略：先看 per_model[currentModel]，没有就用 default；都没就该层不限。
--
-- 可改（与 pricing_versions 不同，pricing 是 append-only；quota 调整不影响历史计费）。
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
-- accounts：主账号 pin / 计费主体元信息（首屏挂 quota_policy 引用）
--
-- 这里的 account 不是子账户，而是主账号 / 计费主体。
-- pin 直接 PK：业务键就是身份键，不引入 BIGINT 中转层。其它表的 account_id
-- VARCHAR(64) 列就是这个 pin（FK → accounts.pin）。
--
-- quota_policy_id NULL = 主账号层不限（M6 跳过主账号层检查）。
-- =====================================================================
CREATE TABLE IF NOT EXISTS accounts (
    pin             VARCHAR(64)  NOT NULL PRIMARY KEY,
    name            VARCHAR(128) NOT NULL,                -- 显示名（"广告业务线" / "搜索业务线" / "默认主账号"）
    enabled         TINYINT(1)   NOT NULL DEFAULT 1,
    quota_policy_id BIGINT UNSIGNED NULL,                 -- NULL = 主账号层不限
    created_at      TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at      TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    deleted_at      TIMESTAMP(6) NULL DEFAULT NULL,
    INDEX idx_quota_policy (quota_policy_id),
    INDEX idx_deleted_at (deleted_at),
    CONSTRAINT fk_account_quota FOREIGN KEY (quota_policy_id) REFERENCES quota_policies(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- seed default account（gateway 启动时如果有 default account 用）
INSERT IGNORE INTO accounts (pin, name) VALUES ('default', 'Default Account');

-- =====================================================================
-- model_services：M5 ModelService middleware 查询的"全局模型 catalog"
--
-- **v0.3 改动**：
--   - 删 account_id（catalog 全局共享，可见性由 account_model_subscriptions 决定）
--   - 删 group_name（M5 不查 group；group 是 endpoint 维度概念）
--   - 删 spec_detail（无消费者；将来真要按 model 配 spec 再走 typed struct）
--   - UNIQUE 改 (service_id), (model)（无主账号分区）
-- =====================================================================
CREATE TABLE IF NOT EXISTS model_services (
    id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    service_id   VARCHAR(191) NOT NULL,                   -- "openai/gpt-4o" / "ark/deepseek-v3-2"；业务/审计分组
    model        VARCHAR(191) NOT NULL,                   -- 用户请求 body 里的 model 字段值（canonical name）

    created_at   TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at   TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    deleted_at   TIMESTAMP(6) NULL DEFAULT NULL,

    UNIQUE KEY uk_service_id (service_id),
    UNIQUE KEY uk_model (model),
    INDEX idx_deleted_at (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- =====================================================================
-- account_model_subscriptions：主账号 × model_services 的可见性 N:M 表
--
-- 没订阅的主账号请求该 model → M5 返回 403 "model not subscribed"。
-- =====================================================================
CREATE TABLE IF NOT EXISTS account_model_subscriptions (
    id                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
   account_id         VARCHAR(64)  NOT NULL,              -- 主账号 pin → accounts.pin
    model_service_id  BIGINT UNSIGNED NOT NULL,           -- → model_services.id
    enabled           TINYINT(1)   NOT NULL DEFAULT 1,    -- 软禁用（保留订阅记录但禁用）

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
-- endpoints：M7 Schedule middleware 选路的"全局上游接入点"
--
-- **v0.3 改动**：删 account_id（平台运营者统一管理上游；BYOK 等真要做时
-- 加 nullable account_id 列）。UNIQUE 改 (name) 全局唯一。
--
-- 核心列只放调度选路 hot path 用得到的；vendor-specific 全进 typed JSON。
-- =====================================================================
CREATE TABLE IF NOT EXISTS endpoints (
    id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    name         VARCHAR(128) NOT NULL,                    -- 业务名（运维 / 监控显示）
    vendor       VARCHAR(32)  NOT NULL,                    -- openai|anthropic|gemini|bedrock|vertex|azure-openai|ark
    protocol     VARCHAR(32)  NOT NULL,                    -- openai|anthropic|gemini|responses|... 上游说什么协议（v0.6 加；endpoint 级属性，不再 vendor 级）
    model        VARCHAR(191) NOT NULL,                    -- M7 选路用
    group_name   VARCHAR(64)  NOT NULL DEFAULT 'default',  -- endpoint 池分组
    weight       INT UNSIGNED NOT NULL DEFAULT 100,
    enabled      TINYINT(1)   NOT NULL DEFAULT 1,

    -- typed JSON：schema 由 Go struct 决定（pkg/repo/{auth,routing,quota}_config.go）
    -- auth 列存的是 AES-GCM 密文（"v1:" + base64），不是合法 JSON，所以用 VARCHAR；
    -- 其它列还是 JSON 让 MySQL 做形态校验。
    auth         VARCHAR(2048) NOT NULL,                   -- AuthConfig 密文 (Scanner/Valuer 在 Go 端 encrypt/decrypt)
    routing      JSON          NOT NULL,                   -- RoutingConfig (url/region/project/...)
    quota        JSON          DEFAULT NULL,               -- 上游硬约束 quota（区别于 quota_policies；这个是 vendor-side 限制）
    capabilities JSON          DEFAULT NULL,               -- EndpointCapabilities: modalities + self_hosted + prefix_cache_enabled 等
    quirks       JSON          DEFAULT NULL,               -- 端点级 body / header 微调 DSL（pkg/protocol/quirks）
    extra        JSON          DEFAULT NULL,

    created_at   TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at   TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    deleted_at   TIMESTAMP(6) NULL DEFAULT NULL,

    UNIQUE KEY uk_name (name),
    INDEX idx_model_group (model, group_name),
    INDEX idx_deleted_at (deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- v0.7：给已存在 endpoints 表加 quirks 列（pkg/protocol/quirks 配置存这）。
-- MySQL 8.0.29+ 支持 ADD COLUMN IF NOT EXISTS，让 schema 演进幂等。
-- 老库重启 gateway 时自动加列；新库 CREATE TABLE 时已带，ADD 是 no-op。
ALTER TABLE endpoints ADD COLUMN IF NOT EXISTS quirks JSON DEFAULT NULL;

-- =====================================================================
-- api_keys：M2 Auth middleware 凭证查表
--
-- **v0.3 改动**：加 quota_policy_id 列（API key 级限流；与主账号级 accounts.quota_policy
-- 叠加）。account_id FK → accounts.pin。
-- =====================================================================
CREATE TABLE IF NOT EXISTS api_keys (
    id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
   account_id       VARCHAR(64)  NOT NULL DEFAULT 'default', -- 主账号 pin / 计费主体
    api_key_hash    CHAR(64)     NOT NULL,                 -- SHA-256(sk-XXX) 的 hex
    api_key_prefix  VARCHAR(16)  NOT NULL,                 -- "sk-abc1de2f" 显示用
    api_key_id      VARCHAR(64)  NOT NULL,                 -- 审计稳定 ID（如 ak_alice_xxxxx）
    name            VARCHAR(64)  NOT NULL DEFAULT '',      -- key 可读标签：prod/dev/ci-bot
    sub_account_id         VARCHAR(64)  NOT NULL,                 -- 子账户 / 操作者
    group_name      VARCHAR(64)  NOT NULL DEFAULT 'default',
    external_user   TINYINT(1)   NOT NULL DEFAULT 0,
    enabled         TINYINT(1)   NOT NULL DEFAULT 1,
    expires_at      TIMESTAMP(6) NULL DEFAULT NULL,
    last_used_at    TIMESTAMP(6) NULL DEFAULT NULL,
    revoked_at      TIMESTAMP(6) NULL DEFAULT NULL,
    quota_policy_id BIGINT UNSIGNED NULL,                  -- NULL = key 层不限；非 NULL 与主账号 quota 叠加

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
-- pricing_versions：M5 ModelService middleware 在请求路径上做"价格快照"
--
-- **append-only**：rule_json 一旦发布 NEVER UPDATE。改价 = 一次事务封盘旧 + insert 新。
-- 详见前一版 docstring；这版只加了 FK → accounts.pin。
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

