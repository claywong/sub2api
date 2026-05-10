-- 136_ops_alert_notify_feishu.sql
-- Adds Feishu (Lark) webhook notification support to the ops alert system.
-- Migration is idempotent.

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '10min';

-- Add notify_feishu flag to alert rules
ALTER TABLE ops_alert_rules
    ADD COLUMN IF NOT EXISTS notify_feishu BOOLEAN NOT NULL DEFAULT false;

-- Add feishu_sent tracking to alert events
ALTER TABLE ops_alert_events
    ADD COLUMN IF NOT EXISTS feishu_sent BOOLEAN NOT NULL DEFAULT false;
