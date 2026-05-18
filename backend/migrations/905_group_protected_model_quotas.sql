-- 私有扩展（不属于 upstream sub2api）
-- 为 groups 表添加 protected_model_quotas 字段
-- 作用：配合 protected_models 列表，为每个受保护模型配置独立的日/周额度上限。
--   key: 模型匹配模式（与 protected_models 中的元素对应）
--   value: {"daily_limit_usd": 5.0, "weekly_limit_usd": 20.0}（字段均可选）
-- merge 策略：upstream 不含此字段，merge 时保留此文件即可

ALTER TABLE groups ADD COLUMN IF NOT EXISTS protected_model_quotas jsonb NOT NULL DEFAULT '{}'::jsonb;
