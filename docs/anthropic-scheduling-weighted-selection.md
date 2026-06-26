# Anthropic 调度：性价比加权选号方案

> 作者：ClaudeCode
> 状态：已实施（默认关闭，`gateway.scheduling.weighted_selection.enabled` 开启）
> 前置阅读：`anthropic-account-scheduling.md`、`anthropic-scheduling-health-aware.md`

---

## 1. 目标与原则

**目标：在同一 Priority 内，按"性价比"选号——质量越好、成本越低的账号拿越多新会话。**

设计约束（均为硬性前提）：

| 约束 | 含义 |
|------|------|
| 粘性会话是刚需 | 已绑定会话的路由一律不动，流量分配的唯一决策点是**新会话建立时** |
| Priority 是人工意图 | 成本分层/套餐分层由人工 Priority 表达，算法只在同 Priority 内裁决 |
| 负载影响不大 | LoadRate 降级为硬约束（满载剔除），不参与排序 |
| HealthVerdict 三态保留 | 加权选号解决"好的多拿"，三态继续解决"坏的别拿"，二者正交 |

## 2. 现状问题（为什么分桶机制达不到目标）

当前 Layer 2 排序链 `Priority → TTFT桶 → OTPS桶 → CacheHit桶 → LoadRate → LRU` 的三个结构性缺陷：

1. **分桶太粗**：TTFT < 4s 全是 0 桶、OTPS ≥ 60 全是 0 桶，健康账号挤在同一桶，
   裁决权实际落到 LoadRate/LRU 手里，等价于均分；
2. **无比例分配**：桶间赢者通吃、桶内 LRU 轮询，不存在"好账号拿 60%、普通拿 40%"；
3. **冷启动振荡**：无样本 = 最优桶，劣质账号被降级 → 不接流量 → 10min 窗口滑空 →
   样本清零 → 复活回最优桶 → 再被降级，周期性回流。

线上实证（某分组三账号）：综合最优的账号（TTFT 5.5s / OTPS 35.6 / $0.021）只拿到 11% 流量，
而 OTPS 仅 21.8 的账号拿了 85%；TTFT 13.5s、成本 15~20 倍（$0.43）的账号仍持续分到流量。

## 3. 方案总览：一行公式

只替换 Layer 2 漏斗中"分桶过滤 + LoadRate + LRU"这一段，其余所有层
（Layer 0 粘性预取 / Layer 1 模型路由 / Layer 1.5 粘性会话 / HealthVerdict 三态 /
各项 schedulable 过滤）原样不动。

```
Layer 2 候选集（已通过全部过滤 + HealthVerdict + LoadRate）
        │
        ▼
按 Priority 取最小值组（沿用 filterByMinPriority）
        │
        ▼
为每个候选算一个权重：

    weight = quality / effRate^β

    quality = (1 - errRate) × ttftFactor × otpsFactor       // 0~1
    effRate = rate × (1 - 0.9 × shrunkHit)                  // 期望单 token 成本
        │
        ▼
按 weight 加权随机抽一个 → tryAcquireAccountSlot
   失败则按权重继续抽下一个（沿用现有重试框架）
```

**就这一行**。没有"体验相近带"、没有 InBand 标记、没有探索 floor 显式参数——
连续权重函数天然兼顾质量倾斜与探索机会。

## 4. quality（质量分）

```
quality = (1 - errRate) × ttftFactor × otpsFactor

ttftFactor = clamp01(1 - p90 / 15000ms)    // P90 ≥ 15s 失分为 0
otpsFactor = clamp01(otpsAvg / 80)         // OTPS ≥ 80 tokens/s 满分 1
successRate = 1 - errRate                  // 账号级 10min 健康窗口错误率
```

**关键设计**：

- **乘法组合，不是加权和**：三项都要好 quality 才高，任何一项拉胯都会拖垮整体——
  比加权和更符合用户对"质量"的直观感受。
- **无样本各项取 1**：冷启动 quality = 1.0，给新账号充分探索机会。
  比"中性分 0.5"更激进，但配合健康检查兜底，差账号会迅速被错误率拉下来。
- **cache hit 不进 quality**：缓存命中率有自增强偏差（拿到越多粘性流量命中率越高），
  只折进 effRate（成本侧）。
- **TTFT P90 不是 avg**：直接用 P90 惩罚长尾。无 P90 样本时退化用 avg。
- **数据源**：TTFT/OTPS 来自 `AccountModelQualityCache` 的 `(account_id, model)` 滑动窗口
  （默认 60min）；错误率来自账号级健康窗口（10min，HealthVerdict 同源）。

## 5. effRate（有效成本）

```
effRate = rate × (1 - 0.9 × shrunkHit)
shrunkHit = hit × N / (N + 20)             // 样本量收缩，缓解少样本虚高
```

**关键设计**：

- **0.9 不是魔法数**：对应 Anthropic 真实定价——cache_read tokens ≈ 0.1× fresh tokens 价格。
  effRate 物理含义是"期望单 token 价格"。
- **样本量收缩 K=20**：防止"2 条样本命中 1 条 = 50% 命中率"的虚高。N 越大、收缩越弱。
- **无缓存样本时 effRate = rate**：无折扣信息时按原始倍率计。
- **下限保护**：`effRate ≥ 0.05`，防止 0 倍率或满命中触发除零。

## 6. β（cost_aggressiveness）成本敏感度

| β 取值 | 行为 |
|---|---|
| β = 0 | 完全不看成本，仅按 quality 抽 |
| β = 1（默认）| 性价比 = quality / effRate |
| β > 1 | 更偏好便宜账号 |
| β → ∞ | 极度成本敏感，最便宜账号通吃 |

## 7. 数字感受

```
账号 A：quality=0.90, effRate=2.0  → w = 0.45
账号 B：quality=0.86, effRate=0.22 → w = 3.91   ← 缓存高+倍率低
账号 C：quality=0.60, effRate=0.46 → w = 1.31

抽样概率：A 8% | B 70% | C 23%
```

**B 又好又便宜（尤其缓存命中率高）吃大头，A 贵但优质有一份，C 质量差被压到 23% 但保留探索机会。**

## 8. 实现落点（遵循 fork companion 约定）

| 文件 | 性质 | 内容 |
|------|------|------|
| `service/gateway_service_weighted_select.go` | **新建 companion** | quality 计算、effRate 计算、加权随机选号主函数 |
| `service/account_model_quality_cache.go` | 私有文件，直接改 | 60min 窗口；P90 估计（每桶保留有限样本） |
| `service/gateway_service.go` | inline 最小修改 | Layer 2 漏斗 1 个 hook：`weighted_selection.enabled` 时走新选号 |
| `config/config.go` | 纯追加 | 新增 `WeightedSelectionConfig` struct，挂在 scheduling 下 |

配置（极简版，仅 3 项）：

```yaml
gateway:
  scheduling:
    weighted_selection:
      enabled: false               # 总开关，默认关闭走原分桶漏斗
      quality_window_minutes: 60   # 质量窗口（默认 60min）
      cost_aggressiveness: 1.0     # β，默认 1.0
```

**写死在代码里的常量**（调参意义不大，藏在 const 减少配置噪音）：

| 名称 | 值 | 含义 |
|---|---|---|
| `wsTTFTZeroMs` | 15000 | TTFT P90 ≥ 15s → ttftFactor = 0 |
| `wsOTPSFullScore` | 80 | OTPS ≥ 80 tokens/s → otpsFactor = 1 |
| `wsCacheDiscount` | 0.9 | 缓存折扣强度（对应 Anthropic cache_read 定价） |
| `wsCacheHitShrinkK` | 20 | 命中率样本量收缩系数 |
| `wsMinRateMultiplier` | 0.05 | 倍率/effRate 下限 |

不新增 DB 表、不新增 Redis key、不改粘性路径、不改 Layer 0/1/1.5、不改 HealthVerdict。

## 9. 极端场景行为

| 场景 | 行为 |
|------|------|
| 所有候选都很优秀 | quality 接近 → β 主导，按成本分 |
| 所有候选都很差 | quality 接近 → β 主导，按成本分 |
| 错误率 100% 的账号 | quality = 0 → weight = 0 → 完全不被选 |
| 所有候选 quality = 0 | 总权重 0 → 均匀随机兜底（不卡死） |
| 0 倍率账号 | rate clamp 到 0.05，weight 大但有界 |
| 单候选 | 直接返回，跳过打分 |

## 10. 可观测性

选号时打 debug 日志，记录：候选数、最终中签账号、quality / rate / effRate / weight。
`log_score_details=true` 时展开每个候选的完整明细（含 TTFT P90 / OTPS / 错误率 / 缓存命中率）。
便于灰度期核对流量分布是否符合预期。

## 11. 灰度与回滚

1. 默认 `enabled=false`，行为与现状完全一致；
2. 选一个账号数多、流量大的 Anthropic 分组开启，观察 1~2 天：
   新会话分布是否向高性价比账号倾斜、低质账号是否还有少量探索流量、均次成本是否下降；
3. 异常时关开关即回滚，无状态残留（质量缓存本就是进程内存）。

## 12. 与旧"划带方案"的差异（历史记录）

旧方案核心概念：
- **三维加权分** `0.4×ttft + 0.35×otps + 0.25×err` + 三个独立归一化阈值
- **划带** `bestScore - ε` 圈出"体验相近带"
- **带内成本权重** `(1/rate)^β`、带外**探索 floor** `0.05 × 带内最大权重`
- **配置项 13 个**

新方案：
- **质量一项** `quality = (1-err) × ttft × otps`（乘积，不归一化）
- **无带概念**，连续权重 `quality / effRate^β`
- **缓存命中折成本** `effRate = rate × (1 - 0.9 × hit)`，恒开
- **配置项 3 个**

行为上等价或更优：
- 高分账号仍吃大头（quality 差 0.3 时 weight 差 ~10x）
- 低分账号仍有探索（连续函数天然非零，不需要显式 floor）
- 冷启动账号探索机会更大（quality=1，不是 0.5）
- 设计/调参/解释复杂度大幅下降

## 13. 后续可选增强（本期不做）

- 质量统计落 Redis + 长短双窗口（1h EWMA + 10min），解决重启清零、多实例割裂
- 粘性会话数与质量分联动（限制低分账号的 max_sessions 上限），让存量流量缓慢迁移
