-- v0.1 schema：sqlite 方言，所有 DDL 用 IF NOT EXISTS 保证 Migrate 幂等。
-- 后续接入 Postgres 时按需拆分 schema_postgres.sql；当前 CRUD 简单到双方言基本一致。

-- =====================================================================
-- model_services：M5 ModelService middleware 查询的"模型路由配置"
-- 主键 id 自增；查询入口是 model（客户端传过来的模型名）。
-- =====================================================================
CREATE TABLE IF NOT EXISTS model_services (
    id           INTEGER PRIMARY KEY,
    service_id   TEXT      NOT NULL UNIQUE,                          -- 业务唯一标识，如 "openai/gpt-4o"
    model        TEXT      NOT NULL UNIQUE,                          -- 客户端可见的模型名，如 "gpt-4o"
    update_time  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,       -- 与 id 共同构成 PricingSnapshot 指纹
    spec_detail  TEXT      NOT NULL DEFAULT '',                      -- JSON: 计量计价详细规格
    group_name   TEXT      NOT NULL DEFAULT 'default',               -- 默认 endpoint 组
    tpm          INTEGER   NOT NULL DEFAULT 0,                       -- 默认每分钟 token 限额
    rpm          INTEGER   NOT NULL DEFAULT 0                        -- 默认每分钟请求数限额
);

-- =====================================================================
-- endpoints：M7 Schedule middleware 选路的"上游接入点"
-- 主键 id 是 caller 提供的字符串（如 "openai_main"）。
-- 查询热路径：按 (model, group_name) 选第一个匹配。
-- =====================================================================
CREATE TABLE IF NOT EXISTS endpoints (
    id            TEXT    PRIMARY KEY,                               -- 业务侧手起的 ID，如 "openai_main"
    vendor        TEXT    NOT NULL,                                  -- 与 adapter.Vendor 对应
    url           TEXT    NOT NULL,                                  -- 上游 base URL
    api_key       TEXT    NOT NULL DEFAULT '',                       -- 凭证；明文存（v0.1）；prod 应外置 KMS
    group_name    TEXT    NOT NULL DEFAULT 'default',                -- 与 UserIdentity.Group 匹配
    model         TEXT    NOT NULL,                                  -- 该 endpoint 服务的模型名
    weight        INTEGER NOT NULL DEFAULT 100,                      -- 加权随机的基础权重
    rpm           INTEGER NOT NULL DEFAULT 0,                        -- endpoint 层每分钟请求数硬上限
    tpm           INTEGER NOT NULL DEFAULT 0,                        -- endpoint 层每分钟 token 硬上限
    rps           INTEGER NOT NULL DEFAULT 0,                        -- endpoint 层每秒请求数硬上限
    capabilities  TEXT    NOT NULL DEFAULT '{}',                     -- JSON: EndpointCapabilities
    extra         TEXT    NOT NULL DEFAULT ''                        -- JSON: 厂商专有配置，Adapter 自行解析
);

CREATE INDEX IF NOT EXISTS idx_endpoints_model_group ON endpoints(model, group_name);
