# Anthropic 调度:质量加权选号方案(最终设计)

> 作者:ClaudeCode
> 状态:已实施(默认关闭,`gateway.scheduling.weighted_selection.enabled` 开启)
> 前置阅读:`anthropic-account-scheduling.md`、`anthropic-scheduling-health-aware.md`

---

## 1. 目标与原则

**目标:在同一 Priority 内,质量最好的账号拿最多的新会话(从而拿最多流量);
体验相近时,成本(账号倍率)最低的优先。**

设计约束(均为硬性前提,方案不得违反):

| 约束 | 含义 |
|------|------|
| 粘性会话是刚需 | 已绑定会话的路由一律不动,流量分配的唯一决策点是**新会话建立时** |
| Priority 是人工意图 | 成本分层/套餐分层由人工 Priority 表达,算法只在同 Priority 内裁决 |
| 体验优先,成本次之 | 成本不得与质量混合加权,只在"体验相近带"内做裁决 |
| 负载影响不大 | LoadRate 降级为硬约束(满载剔除),不参与排序 |
| HealthVerdict 三态保留 | 加权选号解决"好的多拿",三态继续解决"坏的别拿",二者正交 |

## 2. 现状问题(为什么分桶机制达不到目标)

当前 Layer 2 排序链 `Priority → TTFT桶 → OTPS桶 → CacheHit桶 → LoadRate → LRU` 的三个结构性缺陷:

1. **分桶太粗**:TTFT < 4s 全是 0 桶、OTPS ≥ 60 全是 0 桶,健康账号挤在同一桶,
   裁决权实际落到 LoadRate/LRU 手里,等价于均分;
2. **无比例分配**:桶间赢者通吃、桶内 LRU 轮询,不存在"好账号拿 60%、普通拿 40%";
3. **冷启动振荡**:无样本 = 最优桶,劣质账号被降级 → 不接流量 → 10min 窗口滑空 →
   样本清零 → 复活回最优桶 → 再被降级,周期性回流。

线上实证(某分组三账号):综合最优的账号(TTFT 5.5s / OTPS 35.6 / $0.021)只拿到 11% 流量,
而 OTPS 仅 21.8 的账号拿了 85%;TTFT 13.5s、成本 15~20 倍($0.43)的账号仍持续分到流量。

## 3. 方案总览

只替换 Layer 2 漏斗中"分桶过滤 + LoadRate + LRU"这一段(第 2~6 步),
其余所有层(Layer 0 粘性预取 / Layer 1 模型路由 / Layer 1.5 粘性会话 / HealthVerdict 三态 /
各项 schedulable 过滤)原样不动。

```
Layer 2 候选集(已通过全部过滤 + HealthVerdict)
        │
        ▼
按 Priority 取最小值组(沿用 filterByMinPriority,人工意图最高)
        │
        ▼
剔除满载账号(LoadRate >= 100,硬约束)
        │
        ▼
① 算连续质量分 score(account+model 维度滑动窗口)
        │
        ▼
② 划"体验相近带":score >= bestScore - ε 的账号入带
        │
        ▼
③ 带内按成本加权:weight = (1 / 账号倍率)^β
   带外账号:weight = floorRatio × 带内最大权重(探索保底)
        │
        ▼
④ 加权随机选号 → tryAcquireAccountSlot → checkAndRegisterSession
   失败则按权重继续抽下一个,直至成功或耗尽(沿用现有重试框架)
```

## 4. 连续质量分

数据源沿用 `AccountModelQualityCache` 的 `(account_id, requested_model)` 滑动窗口,
新增 TTFT P90 估计(窗口内简单分位数,见 §7)。

**窗口长度与健康窗口解耦**:质量窗口从当前与健康缓存共用的 10min(10 桶 × 1min)
拉长为 **60min(12 桶 × 5min)**,配置项 `quality_window_minutes` 默认 60。
理由:10min 窗口下低流量账号(如日均 124 次 ≈ 12min/条样本)大部分时间无样本,
score 反复跌回中性值,加权随机退化为乱抽;质量是缓变属性,60min 能让仅拿 floor
探测流量的账号也攒够样本。HealthVerdict 的故障检测窗口保持 10min 不变(故障要快进快出)。

```
score = 0.40 × ttftScore + 0.35 × otpsScore + 0.25 × errScore

ttftEff   = 0.5 × ttftAvg + 0.5 × ttftP90          // 惩罚长尾
ttftScore = clamp01(1 - (ttftEff - 1000ms) / 9000ms) // 1s 满分,10s 零分
otpsScore = clamp01(otpsAvg / 80)                    // 80 tokens/s 封顶
errScore  = 1 - errRate                              // 账号级 10min 窗口错误率
```

设计要点:

- **CacheHit 不参与打分**。缓存命中率有自增强偏差(拿到越多粘性流量命中率越高),
  衡量的一半是流量形态而非渠道质量;
- **冷启动给中性分 0.5**,不是最优分。新账号靠探索 floor 自然积累样本归位;
- 各指标无样本时该项取 0.5 中性值,按剩余权重归一化;
- **定时测试数据不参与质量打分**(维持现状:测试只写账号级健康窗口供 HealthVerdict 使用)。
  测试是小 prompt 合成请求,TTFT 偏快、OTPS/CacheHit 无效、模型 key 与真实请求对不齐,
  混入会虚高低流量账号的分数。低流量账号的样本补给由探索 floor + 60min 窗口负责。

## 5. 成本裁决(账号倍率)

- **只用账号倍率(静态配置属性),不用统计均次成本**——后者被请求大小、缓存形态污染;
- 成本只在"体验相近带"内说话:

```
带内:weight_i = (1 / rate_i)^β        // β 默认 1.0
带外:weight_i = floorRatio × max(带内 weight)   // floorRatio 默认 0.05
```

体验拉开差距(score 差 > ε)→ 成本无发言权;体验咬得很紧 → 便宜的多拿。

## 6. 加权随机与流量形态

带内按 `weight` 加权随机抽号(替代确定性 LRU)。陡峭度由 ε 与 β 共同控制:

| 参数 | 默认 | 调大效果 |
|------|------|---------|
| ε(band_epsilon) | 0.10 | 更多账号被视为"体验相近",成本话语权变大 |
| β(cost_beta) | 1.0 | 带内成本倾斜更陡 |
| floorRatio(explore_floor) | 0.05 | 劣质/新账号探测流量更多 |

预期效果(§2 实证数据代入,ε=0.10):账号 2 与账号 1 同带,账号 2 成本更低拿最多新会话;
账号 3 掉出带外,只保留 ~5% 探测流量。

探索 floor 同时消解冷启动振荡:劣质账号始终有少量样本流入,score 连续衰减/恢复,
不再出现"窗口滑空 → 跳回最优"的振荡。

## 7. 实现落点(遵循 fork companion 约定)

| 文件 | 性质 | 内容 |
|------|------|------|
| `service/gateway_service_weighted_select.go` | **新建 companion** | score 计算、相近带划分、成本权重、加权随机选号主函数 |
| `service/account_model_quality_cache.go` | 私有文件,直接改 | 窗口与健康缓存解耦(自有桶常量,60min);增加 TTFT 样本保留(P90 估计:每桶保留有限样本或 P² 算法) |
| `service/gateway_service.go` | inline 最小修改 | Layer 2 漏斗处加 1 个 hook:`weighted_selection.enabled` 时走新选号函数,否则走原分桶漏斗 |
| `config/config.go` | 纯追加 | 新增 `WeightedSelectionConfig` struct,挂到 scheduling 下 |

配置(零值默认 = 完全不启用,upstream 配置不写不受影响):

```yaml
gateway:
  scheduling:
    weighted_selection:
      enabled: false        # 总开关,默认关闭走原分桶漏斗
      quality_window_minutes: 60
      band_epsilon: 0.10
      cost_beta: 1.0
      explore_floor: 0.05
      ttft_full_score_ms: 1000
      ttft_zero_score_ms: 10000
      otps_full_score: 80
      w_ttft: 0.40
      w_otps: 0.35
      w_err: 0.25
```

不新增 DB 表、不新增 Redis key、不改粘性路径、不改 Layer 0/1/1.5、不改 HealthVerdict。

## 8. 可观测性

选号时打 debug 日志(沿用 `logSchedulerSelected` 风格),记录:候选数、带内账号及
score/倍率/权重、最终中签账号、是否 floor 中签。便于灰度期核对流量分布是否符合预期。

## 9. 灰度与回滚

1. 默认 `enabled=false`,行为与现状完全一致;
2. 选一个账号数多、流量大的 Anthropic 分组开启,观察 1~2 天:
   新会话分布是否向高分账号倾斜、floor 账号是否仍有样本、均次成本是否下降;
3. 异常时关开关即回滚,无状态残留(score 缓存本就是进程内存)。

## 10. 后续可选增强(本期不做)

- 质量统计落 Redis + 长短双窗口(1h EWMA + 10min),解决重启清零、多实例割裂;
- 粘性会话数与质量分联动(限制低分账号的 max_sessions 上限),让存量流量缓慢迁移。
