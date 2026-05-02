-- v0.1 schema：MySQL 8.0+ 方言。
-- 全部 DDL 用 IF NOT EXISTS 保证 Migrate 幂等，反复 Run 不报错。
-- charset / collation 统一 utf8mb4 / utf8mb4_unicode_ci，避免 emoji / 中文索引问题。
--
-- **多租户**：所有业务表都带 tenant_id（默认 "default"）；v0.1 单租户运行
-- 但 schema 已为多租户铺路（option c：SaaS-friendly schema 但暂不实现 tenant CRUD / 隔离）。
-- 唯一约束 / 索引都按 (tenant_id, ...) 联合，避免不同租户同名冲突 + 命中索引。
--
-- 设计要点：
--   - id：BIGINT UNSIGNED AUTO_INCREMENT；business 唯一标识用复合 UNIQUE（含 tenant_id）
--   - 字符串字段都用 VARCHAR + 明确长度（不用 TEXT，方便走索引）
--   - JSON blob 字段用 MySQL 8.0 原生 JSON 类型，未来 admin 可 JSON_EXTRACT(...)
--   - update_time / created_at：TIMESTAMP(6)，go time.Time 微秒精度可还原

-- =====================================================================
-- model_services：M5 ModelService middleware 查询的"模型路由配置"
-- =====================================================================
CREATE TABLE IF NOT EXISTS model_services (
    id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    tenant_id    VARCHAR(64)  NOT NULL DEFAULT 'default',
    service_id   VARCHAR(191) NOT NULL,
    model        VARCHAR(191) NOT NULL,
    update_time  TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    spec_detail  JSON         DEFAULT NULL,
    group_name   VARCHAR(64)  NOT NULL DEFAULT 'default',
    tpm          BIGINT       NOT NULL DEFAULT 0,
    rpm          BIGINT       NOT NULL DEFAULT 0,
    UNIQUE KEY uk_tenant_service_id (tenant_id, service_id),
    UNIQUE KEY uk_tenant_model (tenant_id, model)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- =====================================================================
-- endpoints：M7 Schedule middleware 选路的"上游接入点"
-- 主键 (tenant_id, id)：不同租户可重名 endpoint id。
-- =====================================================================
CREATE TABLE IF NOT EXISTS endpoints (
    tenant_id     VARCHAR(64)  NOT NULL DEFAULT 'default',
    id            VARCHAR(128) NOT NULL,
    vendor        VARCHAR(64)  NOT NULL,
    url           VARCHAR(512) NOT NULL,
    api_key       VARCHAR(512) NOT NULL DEFAULT '',
    group_name    VARCHAR(64)  NOT NULL DEFAULT 'default',
    model         VARCHAR(191) NOT NULL,
    weight        INT          NOT NULL DEFAULT 100,
    rpm           BIGINT       NOT NULL DEFAULT 0,
    tpm           BIGINT       NOT NULL DEFAULT 0,
    rps           BIGINT       NOT NULL DEFAULT 0,
    capabilities  JSON         DEFAULT NULL,
    extra         JSON         DEFAULT NULL,
    PRIMARY KEY (tenant_id, id),
    INDEX idx_endpoints_tenant_model_group (tenant_id, model, group_name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- =====================================================================
-- api_keys：M2 Auth middleware 凭证查表
-- api_key 全局唯一（gateway 拿到 key 没有 tenant 上下文，要从 key 反查 tenant）
-- (tenant_id, api_key_id) 联合唯一（同租户内审计 ID 不重）
-- =====================================================================
CREATE TABLE IF NOT EXISTS api_keys (
    id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    tenant_id     VARCHAR(64)  NOT NULL DEFAULT 'default',
    api_key       VARCHAR(255) NOT NULL,                   -- "sk-xxx" 明文（v0.1 不 hash）
    api_key_id    VARCHAR(64)  NOT NULL,                   -- 审计稳定 ID（如 ak_alice_xxxxx）
    user_id       VARCHAR(64)  NOT NULL,                   -- 业务用户 ID
    group_name    VARCHAR(64)  NOT NULL DEFAULT 'default', -- 限流 / 调度分组
    external_user TINYINT(1)   NOT NULL DEFAULT 0,         -- 1 = 第三方付费（走预算检查）
    enabled       TINYINT(1)   NOT NULL DEFAULT 1,         -- 软禁用
    expires_at    TIMESTAMP(6) NULL DEFAULT NULL,          -- NULL = 永不过期
    created_at    TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY uk_api_key (api_key),
    UNIQUE KEY uk_tenant_api_key_id (tenant_id, api_key_id),
    INDEX idx_tenant_user_id (tenant_id, user_id),
    INDEX idx_enabled_expires (enabled, expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
