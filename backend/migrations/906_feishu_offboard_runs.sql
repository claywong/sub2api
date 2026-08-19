-- 906_feishu_offboard_runs.sql
-- 私有迁移（不属于 upstream sub2api）。
--
-- 用途：记录「飞书离职自动禁用」定时任务的每次执行结果。
--
-- 为什么要落库而不是只写 ops job heartbeat：
--   heartbeat 只有一行字符串，回答不了「凭什么禁了这个人」。
--   details 保存每人的判定依据（邮箱、open_id、飞书 status flags、邮箱比对结果），
--   一旦出现误禁能直接查到当时飞书返回了什么，而不是只能猜。
--
-- 幂等说明：
--   - CREATE TABLE / INDEX IF NOT EXISTS，重复执行为 no-op；
--   - 不修改任何 upstream 表结构。

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '10min';

CREATE TABLE IF NOT EXISTS feishu_offboard_runs (
    id                 BIGSERIAL PRIMARY KEY,
    run_at             TIMESTAMPTZ  NOT NULL DEFAULT now(),
    -- cron / manual：区分定时触发与管理员手动触发
    trigger_source     VARCHAR(16)  NOT NULL DEFAULT 'cron',
    -- 空跑模式：只判定不禁用
    dry_run            BOOLEAN      NOT NULL DEFAULT false,
    checked_count      INTEGER      NOT NULL DEFAULT 0,
    -- 判定为已离职的人数
    resigned_count     INTEGER      NOT NULL DEFAULT 0,
    -- 实际执行禁用成功的人数（dry_run 或熔断时为 0）
    disabled_count     INTEGER      NOT NULL DEFAULT 0,
    -- 飞书查不到 / 邮箱对不上，无法判定，一律不禁用
    unverifiable_count INTEGER      NOT NULL DEFAULT 0,
    -- 跳过的人数（admin 角色等）
    skipped_count      INTEGER      NOT NULL DEFAULT 0,
    -- 命中数超过熔断阈值：只告警不执行
    circuit_broken     BOOLEAN      NOT NULL DEFAULT false,
    duration_ms        BIGINT       NOT NULL DEFAULT 0,
    error_message      TEXT,
    -- 每人的判定明细，便于事后追溯误判
    details            JSONB,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- 页面按时间倒序展示最近执行记录
CREATE INDEX IF NOT EXISTS idx_feishu_offboard_runs_run_at
    ON feishu_offboard_runs (run_at DESC);
