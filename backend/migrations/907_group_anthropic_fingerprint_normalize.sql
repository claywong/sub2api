-- 私有扩展（不属于 upstream sub2api）
-- 为 groups 表添加 anthropic_fingerprint_normalize_enabled 字段
-- 作用：开启后，经 Anthropic 协议直通路径（kimi/zhipu/deepseek 的原生
--   Anthropic 端点）转发的出站请求做指纹归一化：
--   1. metadata.user_id 的 device_id/account_uuid 改写为账号级恒定值
--   2. 删除 body.system 中的 x-anthropic-billing-header 块
--   3. User-Agent 归一为规范 claude-cli 值，并兜底剥离 billing header 头
-- merge 策略：upstream 不含此字段，merge 时保留此文件即可

ALTER TABLE groups ADD COLUMN IF NOT EXISTS fingerprint_normalize_enabled boolean NOT NULL DEFAULT false;
