-- 私有扩展（不属于 upstream sub2api）
-- 为 groups 表添加 allow_balance_fallback 字段
-- 作用：订阅额度耗尽后，是否允许自动回退到余额计费
-- merge 策略：upstream 不含此字段，merge 时保留此文件即可

ALTER TABLE groups ADD COLUMN IF NOT EXISTS allow_balance_fallback boolean NOT NULL DEFAULT false;
