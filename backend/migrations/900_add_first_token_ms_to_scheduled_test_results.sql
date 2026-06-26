-- 900_add_first_token_ms_to_scheduled_test_results.sql
-- 私有迁移（不属于 upstream sub2api）。
-- 原文件名：134_add_first_token_ms_to_scheduled_test_results.sql
-- 重命名到 9XX 段，避免与 upstream 后续新增的 134/135/136 ... 编号撞车。
--
-- 幂等说明：
--   - 使用 ADD COLUMN IF NOT EXISTS，已经应用过 134_* 的旧库重新跑此迁移
--     时是 no-op，不会破坏数据；
--   - schema_migrations 表会保留 134_* 旧记录作为历史，无需清理。
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '10min';

ALTER TABLE scheduled_test_results ADD COLUMN IF NOT EXISTS first_token_ms BIGINT;
