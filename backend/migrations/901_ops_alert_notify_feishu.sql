-- 901_ops_alert_notify_feishu.sql
-- 私有迁移（不属于 upstream sub2api）。
-- 原文件名：136_ops_alert_notify_feishu.sql
-- 重命名到 9XX 段，避免与 upstream 后续新增的 136_* 撞车。
--
-- 幂等说明：
--   - 使用 ADD COLUMN IF NOT EXISTS，已经应用过 136_* 的旧库重新跑此迁移
--     时是 no-op，不会改变 BOOLEAN 默认值；
--   - schema_migrations 表会保留 136_* 旧记录作为历史，无需清理。

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '10min';

-- Add notify_feishu flag to alert rules
ALTER TABLE ops_alert_rules
    ADD COLUMN IF NOT EXISTS notify_feishu BOOLEAN NOT NULL DEFAULT false;

-- Add feishu_sent tracking to alert events
ALTER TABLE ops_alert_events
    ADD COLUMN IF NOT EXISTS feishu_sent BOOLEAN NOT NULL DEFAULT false;
