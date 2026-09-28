-- 私有扩展（不属于 upstream sub2api）
-- 移除分组级 anthropic 指纹归一化开关 groups.fingerprint_normalize_enabled
-- 该功能改为账号级配置 account.extra["anthropic_fingerprint_normalize"]
--   （取值 off / claudecode / codex），由转发层直接读账号，不再从分组注入。
-- 存量已开启的分组不自动迁移，需人工在账号侧重新配置（默认 off）。
-- 对应 907 引入的字段；merge 策略：upstream 不含此字段，merge 时保留此文件即可。

ALTER TABLE groups DROP COLUMN IF EXISTS fingerprint_normalize_enabled;
