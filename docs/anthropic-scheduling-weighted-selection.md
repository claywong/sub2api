# Anthropic 调度：性价比选号方案

> 作者：ClaudeCode
> 状态：已实施（默认关闭，`gateway.scheduling.weighted_selection.enabled` 开启）
> 前置阅读：`anthropic-account-scheduling.md`、`anthropic-scheduling-health-aware.md`

---

## v2 修订摘要（当前实施版本）

相对于 v1 的"加权随机"版本，做了三处关键改动，进一步把"调度意图"显性化：

| 维度 | v1（加权随机） | v2（确定性 + 容差带） |
|---|---|---|
| **选号方式** | 按 weight 抽签，每个账号都有概率被中 | 取 score 容差带内**负载最低**的账号（确定性） |
| **quality 公式** | `(1-errRate) × ttftFactor × otpsFactor` | `0.6 × ttftScore + 0.4 × otpsScore`（**去掉 successRate**） |
| **OTPS 阈值** | 80 满分 / 0 → 0 | 50 满分 / 20 → 0（贴合 Claude 实际速度） |
| **冷启动** | 无样本 quality=1，靠加权随机分到流量 | 无缓存样本用候选集**平均缓存率乐观估计**，配合 load=0 自带预热 |
| **配置项** | 3 个 | 4 个（新增 `cost_tolerance`） |

**为什么去掉 successRate**：账号可靠性已经被 **HealthVerdict 三态门禁**（10min 窗口）前置硬过滤——`HealthExcluded` 直接 TempUnschedulable，`HealthStickyOnly` 拒绝新会话。
评分阶段拿到的候选都是"门禁过线"的账号，再算一次 successRate 是职责重叠。
质量评分专注于"活着的账号之间，谁体验更好"。

**为什么改确定性选号**：加权随机最大的问题是**意图被概率稀释**——你说"成本为主"，结果贵账号也有概率被选中，运维无法解释"这次为什么走了贵账号"。
确定性选号让每次决策都可解释（"这是 score 最高带里 load 最低的"），调参方向也明确：
`cost_tolerance` 一个旋钮在"极致成本"和"负载均衡"之间连续滑动。

下文 §3~§12 已按 v2 重写，§13 保留历史 v1 与划带方案的对比。

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

## 3. 方案总览：三步确定性选号

只替换 Layer 2 漏斗中"分桶过滤 + LoadRate + LRU"这一段，其余所有层
（Layer 0 粘性预取 / Layer 1 模型路由 / Layer 1.5 粘性会话 / HealthVerdict 三态 /
各项 schedulable 过滤）原样不动。

```
Layer 2 候选集（已通过全部过滤 + HealthVerdict + LoadRate）
        │
        ▼
① 按 Priority 取最小值组（沿用 filterByMinPriority）
        │
        ▼
② 为每个候选算 score（冷账号用候选集平均缓存率乐观估计 effRate）：

    score   = quality / effRate^β
    quality = 0.6 × ttftScore + 0.4 × otpsScore     // 0~1，体验加权和
    effRate = rate × (1 - 0.9 × shrunkHit)          // 期望单 token 成本
        │
        ▼
③ 取 score 容差带内（score ≥ maxScore × (1 - cost_tolerance)）
   load 最低的账号 → tryAcquireAccountSlot
   失败移出候选，再次进入 ① 重试（沿用现有重试框架）
```

**三个动作**：①优先级过滤 ②性价比打分 ③带内 load 选最空。
没有概率、没有随机数，每次选号都能说一句人话："score 最高带里 load 最低的那个"。

## 4. quality（质量分）

```
quality = 0.6 × ttftScore + 0.4 × otpsScore

ttftScore = clamp01(1 - p90 / 15000ms)           // P90 ≥ 15s → 0 分
otpsScore = clamp01((otpsAvg - 20) / (50 - 20))  // OTPS ≤ 20 → 0 分；≥ 50 → 满分
```

**关键设计**：

- **加权求和，不是相乘**：体验维度（TTFT/OTPS）改加权和——某项拉胯不会一票否决，
  允许"TTFT 稍慢但 OTPS 快"的账号仍拿到合理得分。
  TTFT 权重 0.6 > OTPS 权重 0.4，因为用户对首字节比对输出速度更敏感。
- **不含 successRate**：账号可靠性由 HealthVerdict 门禁前置硬过滤
  （`HealthExcluded` 直接 TempUnschedulable），评分阶段不再重复判定。
- **无样本各项取 1（乐观）**：冷启动 quality = 1.0，配合冷账号 load=0 的天然优势
  快速预热。差账号即使没样本进了池，也会被 ③ 的 load-aware 自然导出。
- **OTPS 阈值 50/20**：v1 的 80 阈值对 Claude Opus（天然慢，~20-30 t/s）系统性低估，
  v2 调整为 50 满分、20 零分，区分度更合理。
- **TTFT P90 不是 avg**：直接用 P90 惩罚长尾。无 P90 样本时退化用 avg。
- **cache hit 不进 quality**：缓存命中率有自增强偏差（拿到越多粘性流量命中率越高），
  只折进 effRate（成本侧）。
- **数据源**：TTFT/OTPS 来自 `AccountModelQualityCache` 的 `(account_id, model)` 滑动窗口
  （默认 60min）；不再读取 healthCache（成功率不参与）。

## 5. effRate（有效成本）+ 冷启动乐观估计

```
effRate = rate × (1 - 0.9 × shrunkHit)
shrunkHit = hit × N / (N + 20)             // 样本量收缩，缓解少样本虚高

冷启动账号（无缓存样本）：
    hit ← 候选集平均缓存率
    N   ← wsColdStartCacheN（20）
```

**关键设计**：

- **0.9 不是魔法数**：对应 Anthropic 真实定价——cache_read tokens ≈ 0.1× fresh tokens 价格。
  effRate 物理含义是"期望单 token 价格"。
- **样本量收缩 K=20**：防止"2 条样本命中 1 条 = 50% 命中率"的虚高。N 越大、收缩越弱。
- **冷启动乐观估计（v2 新增）**：v1 中无缓存样本就 `effRate = rate`，导致冷账号显得"贵"，
  在确定性选号下永远输给老账号，陷入"选不上→没缓存→更选不上"的死循环。
  v2 让冷账号继承候选集平均缓存率（等效样本量 20，配合 K=20 给约半折），温和乐观。
- **全候选都无缓存样本时**：avgHit 不可得，退化为 `effRate = rate`，按 load 决胜。
- **下限保护**：`effRate ≥ 0.05`，防止 0 倍率或满命中触发除零。

## 6. β（cost_aggressiveness）+ cost_tolerance 两个旋钮

| 参数 | 默认 | 调小（→0） | 调大 |
|---|---|---|---|
| **β** cost_aggressiveness | 1.0 | 完全不看成本，纯按 quality | 更偏好便宜账号；→∞ 时最便宜通吃 |
| **cost_tolerance** | 0.15 | 严格只选 score 最高的（确定性，可能热点） | 放宽带宽 → 更多账号入带 → 偏负载均衡；=1 时退化为纯按 load 选 |

两者组合连续覆盖了从"极致成本最优"到"绝对负载均衡"的整个谱系：

```
cost_tolerance → 0   ───  极致省钱、可能热点
cost_tolerance = 0.15 ─  默认：成本主导，等价带内均衡（避免热点 & 冷启动预热）
cost_tolerance → 1   ───  纯负载均衡，忽略成本差异
```

## 7. 数字感受

```
四个账号（默认 β=1.0, cost_tolerance=0.15）：

       quality   rate  cacheHit  effRate  score   load
账号 A   1.00    1.00    0.30     0.73    1.37    50
账号 B   0.88    1.00    0.50     0.55    1.60    40
账号 C   0.95    0.50    0.60     0.27    3.55    80    ← maxScore
账号 D   1.00    1.00    （冷）    0.65*   1.54    0     ← *用候选集均值 0.47 乐观估计

maxScore = 3.55, 容差阈值 = 3.55 × 0.85 = 3.02
入带账号：只有 C（其他都 < 3.02）
→ 选 C（load=80，但带内只此一个）

如果把 cost_tolerance 调到 0.6（阈值 = 1.42）：
入带账号：B (1.60), C (3.55), D (1.54)
带内 load 最低 = D (load=0)
→ 选 D（冷启动账号借负载均衡被预热）
```

**默认参数下成本主导（C 通吃），放宽容差就启用负载均衡（D 上场预热）。** v1 的加权随机里
不管怎么调，A 都有~15% 概率被中——这正是 v2 想消除的"意图被概率稀释"。

## 8. 实现落点（遵循 fork companion 约定）

| 文件 | 性质 | 内容 |
|------|------|------|
| `service/gateway_service_weighted_select.go` | **新建 companion** | quality 计算、effRate 计算（含冷启动乐观）、确定性选号主函数 |
| `service/account_model_quality_cache.go` | 私有文件，直接改 | 60min 窗口；P90 估计（每桶保留有限样本） |
| `service/gateway_service.go` | inline 最小修改 | Layer 2 漏斗 1 个 hook：`weighted_selection.enabled` 时走新选号 |
| `config/config.go` | 纯追加 | 新增 `WeightedSelectionConfig` struct，挂在 scheduling 下 |

配置（4 项）：

```yaml
gateway:
  scheduling:
    weighted_selection:
      enabled: false               # 总开关，默认关闭走原分桶漏斗
      quality_window_minutes: 60   # 质量窗口（默认 60min）
      cost_aggressiveness: 1.0     # β，默认 1.0
      cost_tolerance: 0.15         # 容差带宽，默认 0.15
```

**写死在代码里的常量**（调参意义不大，藏在 const 减少配置噪音）：

| 名称 | 值 | 含义 |
|---|---|---|
| `wsTTFTZeroMs` | 15000 | TTFT P90 ≥ 15s → ttftScore = 0 |
| `wsOTPSZeroScore` | 20 | OTPS ≤ 20 tokens/s → otpsScore = 0（v2 新增） |
| `wsOTPSFullScore` | 50 | OTPS ≥ 50 tokens/s → otpsScore = 1（v1 是 80） |
| `wsQualityTTFTWeight` | 0.6 | quality 中 TTFT 子分权重（v2 新增） |
| `wsQualityOTPSWeight` | 0.4 | quality 中 OTPS 子分权重（v2 新增） |
| `wsCacheDiscount` | 0.9 | 缓存折扣强度（对应 Anthropic cache_read 定价） |
| `wsCacheHitShrinkK` | 20 | 命中率样本量收缩系数 |
| `wsColdStartCacheN` | 20 | 冷启动乐观估计等效样本量（v2 新增） |
| `wsMinRateMultiplier` | 0.05 | 倍率/effRate 下限 |
| `wsCostToleranceDef` | 0.15 | cost_tolerance 默认值（v2 新增） |

不新增 DB 表、不新增 Redis key、不改粘性路径、不改 Layer 0/1/1.5、不改 HealthVerdict。

## 9. 极端场景行为

| 场景 | v2 行为 |
|------|------|
| 所有候选 quality 接近 | β 主导，按成本（effRate）排序 → 最便宜的入带 |
| 所有候选 quality = 0（TTFT≥15s 且 OTPS≤20） | maxScore = 0，阈值 = 0，全部入带 → 退化为按 load 选 |
| 错误率 100% 的账号 | 由 HealthVerdict 门禁拦截，根本不会到评分阶段 |
| 0 倍率账号 | rate clamp 到 0.05，effRate 仍有界 |
| 单候选 | 直接返回，跳过打分 |
| 全部冷启动账号（无缓存样本） | avgHit 不可得，全部按 rate 计 → 按 load 选最空 |
| 一半冷启动 + 一半老账号 | 冷账号继承老账号平均缓存率 → effRate 相近 → 冷账号借 load 优势预热 |

## 10. 可观测性

选号时打 debug 日志，记录：候选数、最终选中账号、quality / rate / effRate / **score / load**。
`log_score_details=true` 时展开每个候选的完整明细
（含 TTFT P90 / OTPS / 缓存命中率 / Rate / EffRate / Score / Load）。

每次决策都可解释为："在 N 个候选中，score 最高带（≥ maxScore × 0.85）里 load 最低的 X 号账号"。
便于灰度期核对：①选号结果是否符合"成本为主"直觉 ②负载是否在性价比相近账号间合理分摊
③冷账号是否得到预热机会。

## 11. 灰度与回滚

1. 默认 `enabled=false`，行为与现状完全一致；
2. 选一个账号数多、流量大的 Anthropic 分组开启，观察 1~2 天：
   新会话分布是否向高性价比账号倾斜、低质账号是否还有少量探索流量、均次成本是否下降；
3. 异常时关开关即回滚，无状态残留（质量缓存本就是进程内存）。

## 12. 历史方案对比

| 维度 | 旧"划带方案" | v1 加权随机 | **v2 确定性 + 容差带（当前）** |
|---|---|---|---|
| quality 计算 | `0.4×ttft + 0.35×otps + 0.25×err`，三独立归一化 | `(1-err) × ttft × otps`，三项相乘 | `0.6 × ttft + 0.4 × otps`，两项加权和 |
| 成功率位置 | 进 quality | 进 quality | **不进 quality**（HealthVerdict 门禁前置） |
| 成本表达 | 带内 `(1/rate)^β`、带外 floor | `quality / effRate^β` 连续权重 | `quality / effRate^β`，加冷启动乐观估计 |
| 选号方式 | 划带 + 探索 floor | 加权随机抽签 | **确定性：容差带内按 load 选最空** |
| 配置项 | 13 个 | 3 个 | 4 个 |
| 可解释性 | 中（带内带外语义割裂） | 弱（"为什么这次走了贵的"） | **强**（"score 最高带里 load 最低的"） |
| 冷启动 | quality 中性分 0.5 | quality=1，靠随机分流量 | quality=1，effRate 用平均缓存率乐观，靠 load 优势预热 |

**v1→v2 演进核心**：
- 把"调度意图"从概率分布升级为确定性规则——运维每次都能解释清选择
- 用 HealthVerdict 门禁吸收成功率维度，评分专注体验+成本，职责不重叠
- 引入 `cost_tolerance` 单旋钮覆盖"成本主导↔负载均衡"完整谱系，参数语义直观

## 13. 后续可选增强（本期不做）

- 质量统计落 Redis + 长短双窗口（1h EWMA + 10min），解决重启清零、多实例割裂
- 粘性会话数与质量分联动（限制低分账号的 max_sessions 上限），让存量流量缓慢迁移
