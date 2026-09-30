-- 私有扩展（不属于 upstream sub2api）
-- 分组级「按模型/模型前缀」倍率系数。
--
-- 背景：composite 等分组对外只有一个统一倍率，但部分模型（如 opus 系列）在专属分组里
--   本来就是更高的倍率。改分组逐模型定价的单价虽然能间接实现，但使用记录里的原价/倍率
--   会对不上，账号侧成本也会被放大。
--
-- 设计：groups.model_rate_multipliers（JSONB 配置），形如
--        {"enabled": true, "rules": [
--           {"match": "claude-opus*", "multiplier": 2},
--           {"match": "gpt-6-astra",  "multiplier": 1.5}
--        ]}
--   计费时 rate_multiplier = (用户专属倍率 ?? 分组倍率) × 命中规则的系数 × 高峰因子，
--   结果只写入 usage_logs.rate_multiplier，不单独记录系数；total_cost 保持官方价。
--   配置随分组读取（Group 已在 api key auth cache 中），计费时零额外查询。
--
-- 详见 docs/group-model-rate-multiplier.md
--
-- merge 策略：upstream 不含此字段，merge 时保留此文件即可

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '10min';

ALTER TABLE groups ADD COLUMN IF NOT EXISTS model_rate_multipliers JSONB NOT NULL DEFAULT '{}'::jsonb;
