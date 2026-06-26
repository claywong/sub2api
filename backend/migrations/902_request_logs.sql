-- 902_request_logs.sql
-- 私有迁移（不属于 upstream sub2api）。
-- 原文件名：137_request_logs.sql
-- 重命名到 9XX 段，避免与 upstream 后续新增的 137_* 撞车。
--
-- 功能：为 request_logs 表创建 schema，存储原始请求/响应内容。
-- 由 gateway.request_log.enabled 配置开关控制（默认 false）。
--
-- 幂等说明：
--   - CREATE TABLE IF NOT EXISTS / CREATE INDEX IF NOT EXISTS，
--     已经应用过 137_* 的旧库重新跑此迁移时是 no-op；
--   - schema_migrations 表会保留 137_* 旧记录作为历史，无需清理。

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '10min';

CREATE TABLE IF NOT EXISTS request_logs (
    request_id     VARCHAR(64)  NOT NULL,
    session_id     VARCHAR(256),
    user_id        BIGINT       NOT NULL,
    request_body   TEXT,
    response_body  TEXT,
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (request_id)
);

CREATE INDEX IF NOT EXISTS idx_request_logs_session_id  ON request_logs (session_id);
CREATE INDEX IF NOT EXISTS idx_request_logs_user_created ON request_logs (user_id, created_at);
