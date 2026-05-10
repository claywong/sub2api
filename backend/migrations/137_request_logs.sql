-- 137_request_logs.sql
-- Adds request_logs table for storing raw request/response bodies.
-- Controlled by gateway.request_log.enabled config flag (default: false).
-- Migration is idempotent.

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
