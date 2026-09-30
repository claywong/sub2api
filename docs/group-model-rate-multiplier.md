# 分组单模型倍率（model_rate_multipliers）设计方案

> 作者：ClaudeCode  
> 日期：2026-09-30  
> 状态：已实现（私有扩展）

## 背景

composite 分组对外只有一个统一倍率，但其中部分模型（如 opus 系列）在专属分组里本来就是更高的倍率。现有能力只能通过「分组逐模型定价」改单价来间接实现，问题是：

- 使用记录里「单价 / 原价」变成手填的虚高价，「倍率」仍显示分组倍率，用户看到的明细对不上；
- 账号侧额度消耗按 `total_cost × account_rate_multiplier` 计算，会被一起放大；
- 分组逐模型定价命中后会跳过渠道定价，只为调倍率而配一条规则会丢掉渠道自定义价。

## 方案对比

| 方案 | 结论 | 原因 |
|------|------|------|
| 改分组逐模型定价的单价 | 否决 | 使用页原价/倍率对不上，账号成本被放大 |
| 模型倍率绝对值覆盖分组倍率 | 否决 | 与用户专属倍率冲突，必须人为规定谁优先，且不随分组倍率联动 |
| 模型系数，与分组倍率相乘 | **采用** | 与用户专属倍率天然可叠加，无优先级问题；使用页口径一致 |

## 计费语义

```
基础倍率        = 用户专属倍率 ?? 分组倍率            // 现有逻辑不变
模型系数        = 命中规则 ? rule.multiplier : 1
rate_multiplier = 基础倍率 × 模型系数 × 高峰因子       // 写入 usage_logs
actual_cost     = total_cost × rate_multiplier
```

示例：分组 1.5，`claude-opus*` 系数 2 → 普通用户 3.0，专属倍率 1.2 的用户 2.4，高峰因子照常叠加。

- 使用记录只存合成后的 `rate_multiplier`，不新增列（与高峰因子的现有做法一致）；`total_cost` 保持官方价。
- 图片/视频按次计费：未开启独立倍率时继承（基础倍率 × 模型系数）；开启 `image_rate_independent` / `video_rate_independent` 时保持现有语义，不乘模型系数。
- 订阅额度、按模型配额、余额扣费都读 `actual_cost`，自动生效。
- 代价：事后无法从单条记录拆分倍率来源，需对照当时的分组配置。

## 配置结构

分组新增字段 `model_rate_multipliers`（JSONB）：

```json
{
  "enabled": true,
  "rules": [
    { "match": "claude-opus*", "multiplier": 2 },
    { "match": "gpt-6-astra",  "multiplier": 1.5 }
  ]
}
```

校验（非法返回 400，错误码 `INVALID_MODEL_RATE_MULTIPLIERS`）：

- `match` 规则与 `model_quotas` 一致：非空、`*` 只能在末尾、禁止裸 `*`、长度 ≤ 200、归一（trim + 小写）后去重；
- `multiplier` 必须是有限数且 > 0（0 会让模型免费；禁用模型请用模型白名单或按模型配额）；
- `enabled=true` 但规则为空返回 400。

## 匹配规则

- 复用 `GroupModelQuotas.MatchRule` 的匹配逻辑：候选名归一（`groupModelAllowlistCandidates`）、精确优先于前缀、前缀间最长优先、同强度先声明者优先。
- 先用请求模型（使用记录「模型」列）匹配，未命中再用实际转发模型匹配。这样 composite 公开别名（如 `claude`）落到 opus 时也能命中 `claude-opus*`。

## 计费接入点

新增 `backend/internal/service/group_model_rate_multiplier.go`，承载类型、domain 转换、匹配、校验和系数解析 helper，避免 `gateway_usage_billing.go`（已 1300+ 行）继续膨胀。

| 入口 | 位置 | 改动 |
|------|------|------|
| Anthropic / Gemini / Antigravity | `gateway_usage_billing.go` `ResolveUserGroupRateMultiplier` 之后、`computePeakAwareMultipliers` 之前 | 基础倍率 × 模型系数 |
| OpenAI / Grok | `openai_gateway_usage.go` 同位置（`baseMultiplier`） | 同上，`videoMultiplier` 随 `baseMultiplier` 继承 |

两个入口都从 `apiKey.Group`（认证快照）读取配置，零额外查询。

## 改动清单

后端：

- 迁移 `backend/migrations/910_group_model_rate_multipliers.sql`：`ALTER TABLE groups ADD COLUMN IF NOT EXISTS model_rate_multipliers JSONB NOT NULL DEFAULT '{}'::jsonb;`，附私有扩展头注释与迁移护栏测试；
- ent schema `group.go` 加字段并重新生成；`internal/domain` 加持久化类型；
- `service.Group` 加字段；`group_repo.go` 创建/更新 setter；`api_key_repo.go` 的 `GetByKeyForAuth` 字段投影与 `groupEntityToService`（漏选会让配置静默不生效）；
- 认证快照 `api_key_auth_cache.go` 结构体与 impl 两处映射，`apiKeyAuthSnapshotVersion` 25 → 26（旧快照反序列化为零值，必须淘汰）；
- 管理端 `CreateGroupRequest` / `UpdateGroupRequest`、`admin_group.go` 创建/更新校验、DTO `AdminGroup` 与 mapper；
- 分组复制 `admin_group_duplicate.go` 深拷贝规则；
- 模型广场 `PlazaModel.RateFactor`：仅命中规则且 ≠1 时设置，接口字段 `rate_factor` 同样只在生效时下发（前端缺省按 1），与计费同一匹配逻辑。
- 按模型配额与单模型倍率共用 `bestModelRuleIndex`（命中）与 `validateModelRuleMatch`（match 校验）。
- `CreateGroup` / `UpdateGroup` 通过 `normalizeGroupModelRules` / `applyGroupModelRuleUpdates` 接入，私有扩展逻辑不继续堆在 upstream 大函数里。

前端：

- 新组件 `components/admin/group/GroupModelRateMultipliersField.vue`（仿 `GroupModelQuotasField.vue`），每条规则实时显示「生效倍率 = 分组倍率 × 系数」；
- `GroupsView.vue` 创建/编辑表单各引用一次并随提交携带（该文件已近 7000 行，只加引用）；
- `types/index.ts`、`i18n/locales/{zh,en}/admin/overview.ts`；
- 模型广场 `PlazaModelPricingTable` 按模型乘系数展示。

测试：

- 匹配优先级（精确/前缀/最长前缀/候选归一/请求模型→转发模型回退）；
- 倍率合成：分组、用户专属、高峰、独立图片/视频倍率，两个计费入口各覆盖；
- 认证快照往返与旧版本淘汰；校验 400 各分支；前端组件单测。

文档：`CLAUDE.md`「5. 分组管理」补充 `model_rate_multipliers` 字段说明与 curl 示例。

## 不在本期范围

- 利润控制准入门：阈值仍按分组倍率计算，不感知模型。系数 > 1 时只会更保守；系数 < 1 时可能放行不赚钱的账号。composite 分组本身不支持利润控制。
- `/v1/sub2api/billing` 查询接口：Key 级接口，没有模型维度，仍返回分组/用户倍率。
- 批量生图（`batch_image_public.go` 的 `resolvePricingSnapshot`）：独立计费路径，仍只用分组/用户倍率。

## 上线注意

- 之前为同一目的在「分组逐模型定价」里改过的单价，要改回官方价，否则会与模型系数叠加计费。
- 部署后认证快照版本变化，旧缓存自动失效，首批请求会回源加载一次分组配置。
