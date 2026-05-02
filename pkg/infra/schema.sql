-- v0.1 schema：MySQL 8.0+ 方言。
-- 全部 DDL 用 IF NOT EXISTS 保证 Migrate 幂等，反复 Run 不报错。
-- charset / collation 统一 utf8mb4 / utf8mb4_unicode_ci，避免 emoji / 中文索引问题。
--
-- 设计要点：
--   - id：BIGINT UNSIGNED AUTO_INCREMENT，business 唯一标识用 UNIQUE 索引（service_id / model）
--   - 字符串字段都用 VARCHAR + 明确长度（不用 TEXT，方便走索引）；
--     VARCHAR(191) 对应 utf8mb4 4 bytes/char × 191 = 764 字节，落在索引前缀长度限制内
--   - JSON blob 字段（spec_detail / capabilities / extra）用 MySQL 8.0 原生 JSON 类型，
--     未来 admin 想按 JSON 字段筛选可以 JSON_EXTRACT(...)
--   - update_time：DEFAULT CURRENT_TIMESTAMP；不加 ON UPDATE，让 Go 代码显式控制 update 语义

-- =====================================================================
-- model_services：M5 ModelService middleware 查询的"模型路由配置"
-- =====================================================================
CREATE TABLE IF NOT EXISTS model_services (
    id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    service_id   VARCHAR(191) NOT NULL,
    model        VARCHAR(191) NOT NULL,
    update_time  TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    spec_detail  JSON         DEFAULT NULL,
    group_name   VARCHAR(64)  NOT NULL DEFAULT 'default',
    tpm          BIGINT       NOT NULL DEFAULT 0,
    rpm          BIGINT       NOT NULL DEFAULT 0,
    UNIQUE KEY uk_service_id (service_id),
    UNIQUE KEY uk_model (model)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- =====================================================================
-- endpoints：M7 Schedule middleware 选路的"上游接入点"
-- 主键 id 是 caller 提供的字符串（如 "openai_main"），不自增。
-- =====================================================================
CREATE TABLE IF NOT EXISTS endpoints (
    id            VARCHAR(128) NOT NULL PRIMARY KEY,
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
    INDEX idx_endpoints_model_group (model, group_name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
