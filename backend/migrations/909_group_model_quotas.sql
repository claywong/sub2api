-- 私有扩展（不属于 upstream sub2api）
-- 分组级「按模型/模型前缀」配额。
--
-- 背景：分组原有 daily/weekly/monthly_limit_usd 是整组共享的总配额，所有模型混算，
--   无法约束单个高价模型或某个模型系列（如 claude-opus* 系列、gpt-6-astra）。
--
-- 设计（两张结构分离，与既有 model_pricing / model_allowlist 的分工一致）：
--   1. groups.model_quotas（JSONB 配置）：规则列表，形如
--        {"enabled": true, "rules": [
--           {"match": "claude-opus*", "daily": 50, "weekly": 200, "monthly": null},
--           {"match": "gpt-6-astra",  "daily": 10}
--        ]}
--      限额随分组配置读取（Group 已在 api key auth cache 中），判定时零额外查询。
--   2. user_group_model_usage（本表）：按 (user_id, group_id, rule_key) 记录三窗口用量。
--
-- 为什么按 (user_id, group_id) 而不是 subscription_id：
--   订阅模式下两者等价，但按 (user, group) 建键在余额模式同样成立，
--   且与 Redis 既有 key 口径 billingSubKey(userID, groupID) 对齐。
--
-- rule_key 存规则原文的归一形式（小写 + trim，如 "claude-opus*"）而非具体模型名，
-- 这样 claude-opus-4-5 与 claude-opus-4-6 共享同一份系列额度。
--
-- 窗口语义与 user_platform_quotas 完全一致：
--   daily  = 配置时区当日 0 点对齐
--   weekly = 配置时区本周起点对齐
--   monthly = 30 天滚动（窗口起点为首次计量时刻，过期后以 now 为新起点）
--
-- merge 策略：upstream 不含此字段与此表，merge 时保留此文件即可

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '10min';

ALTER TABLE groups ADD COLUMN IF NOT EXISTS model_quotas JSONB NOT NULL DEFAULT '{}'::jsonb;

CREATE TABLE IF NOT EXISTS user_group_model_usage (
    id                   BIGSERIAL PRIMARY KEY,
    user_id              BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    group_id             BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,

    -- 命中的规则原文（归一后），非具体模型名；见文件头说明
    rule_key             VARCHAR(200) NOT NULL,

    -- 当前窗口已用量（USD）
    daily_usage_usd      DECIMAL(20,10) NOT NULL DEFAULT 0,
    weekly_usage_usd     DECIMAL(20,10) NOT NULL DEFAULT 0,
    monthly_usage_usd    DECIMAL(20,10) NOT NULL DEFAULT 0,

    -- 窗口起点（NULL = 首次尚未初始化）
    daily_window_start   TIMESTAMPTZ,
    weekly_window_start  TIMESTAMPTZ,
    monthly_window_start TIMESTAMPTZ,

    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at           TIMESTAMPTZ
);

-- 软删除友好唯一索引：同用户同分组同规则只允许一条未删除记录。
-- 落账走 ON CONFLICT ... WHERE deleted_at IS NULL DO UPDATE，依赖此索引。
CREATE UNIQUE INDEX IF NOT EXISTS usergroupmodelusage_user_group_rule_uq
    ON user_group_model_usage (user_id, group_id, rule_key)
    WHERE deleted_at IS NULL;

-- 按 (user, group) 批量读取该分组下所有规则用量（判定与进度展示）
CREATE INDEX IF NOT EXISTS usergroupmodelusage_user_group
    ON user_group_model_usage (user_id, group_id);

CREATE INDEX IF NOT EXISTS usergroupmodelusage_deleted_at
    ON user_group_model_usage (deleted_at);
