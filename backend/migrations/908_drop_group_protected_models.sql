-- 私有扩展（不属于 upstream sub2api）
-- 回收 904 / 905 引入的两个 groups 字段
-- 作用：会话级模型锁定与受保护模型共享额度功能已下线，删除对应列，
--   使 groups 表结构回到 upstream 形态（仅保留其余私有字段）。
--   904_group_protected_models.sql 与 905_group_protected_model_quotas.sql
--   已从本仓库删除，schema_migrations 中的历史记录作为孤儿保留，无需清理。
-- merge 策略：upstream 不含这两列，merge 时保留此文件即可

ALTER TABLE groups DROP COLUMN IF EXISTS protected_models;
ALTER TABLE groups DROP COLUMN IF EXISTS protected_model_quotas;
