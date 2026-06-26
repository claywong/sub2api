-- 私有扩展（不属于 upstream sub2api）
-- 为 groups 表添加 protected_models 字段
-- 作用：会话级模型锁定保护列表（仅 Anthropic 协议）。
--   当一个会话首次使用的 model 不在保护列表内时，后续请求不允许将 model
--   切换为本列表中的任何模型（防止"先 cheap 后 expensive"的上下文白嫖）。
--   列表中的字符串支持 * 通配符，如 "claude-opus-*"。
-- merge 策略：upstream 不含此字段，merge 时保留此文件即可

ALTER TABLE groups ADD COLUMN IF NOT EXISTS protected_models jsonb NOT NULL DEFAULT '[]'::jsonb;
