# Anthropic 账号调度：加权打分 + 滑动窗口健康度

> 作者：ClaudeCode
> 日期：2026-04-29
> 状态：方案待实施
> 关联代码：`backend/internal/service/gateway_service.go`、`backend/internal/service/account_test_health_cache.go`、`backend/internal/handler/gateway_handler.go`

## 1. 背景与目标

### 1.1 背景

当前 Anthropic 账号调度链（`SelectAccountWithLoadAwareness` → Layer 2）使用**字典序硬过滤**：

```
filterByMinPriority → filterByMinLoadRate → filterByMinLatencyBucket → filterByMinSlowBucket → selectByLRU
```

每一步都是 `==` 严格相等，**前置维度淘汰后置维度**。当 `account.Priority` 与渠道倍率挂钩（如 0.10 → 10、0.45 → 45）时：

- `filterByMinPriority` 永远只放行最便宜档（Priority=10）
- 其它档**根本走不到** TTFT / 慢率 / LRU 这几步
- 性能信号在便宜档**只剩 1~2 个账号**时几乎无差异化

实际表现为"价格独裁"：便宜账号繁忙、性能差时仍被持续选中。

### 1.2 目标

1. **价格差不多时**性能优先（TTFT 短、OTPS 高、错误率低、负载合理）
2. **会话粘滞**：选中后除非换会话或账号坏掉，否则同 sessionHash 不切账号
3. **健康度三态**：可粘性会话继续 + 新会话避开（`StickyOnly`），或彻底排除（`Excluded`）
4. **数据融合**：主动 test 与真实调用共用同一份健康视图
5. **可观测**：调度决策有日志、有 metrics、有 admin 快照
6. **可灰度**：配置开关切换新老算法，可按 group 白名单灰度

### 1.3 不在范围

- OpenAI 调度（独立的 `openAIAccountScheduler`，已有加权打分）
- Gemini 调度
- Layer 1（模型路由）的字典序排序（保持现状）
- Sticky session 存储/TTL/清理逻辑（已满足需求）

## 2. 现状分析

### 2.1 调度三层结构（gateway_service.go:1383-1944）

```
Layer 1   模型路由优先（仅 Anthropic + ModelRoutingEnabled）
Layer 1.5 粘性会话（sticky_session:{groupID}:{sessionHash} → accountID, Redis, TTL=1h）
Layer 2   分层硬过滤选择（本方案核心改造点）
Layer 3   兜底排队 WaitPlan
```

### 2.2 健康缓存现状（account_test_health_cache.go）

| 字段 | 主动 test 写 | 真实调用写 | 调度读取 |
|---|:---:|:---:|---|
| `TTFTEwma` | ✅ `UpdateFromTest` | ✅ `UpdateTTFT` | `LatencyBucket` |
| `SlowReqCount`/`TotalReqCount` (10min) | ✅ | ✅ `UpdateDuration` | `SlowBucket` |
| `ConsecFails` | ✅ | ❌ | `PassesHardFilter` |
| `LastStatus`/`LastTestedAt` | ✅ | ❌ | — |
| `RetryInterval`/`NextRetryAt` | ✅ | ❌ | test runner 自身 |

**已融合**：TTFT、慢率
**未融合**：错误率（真实调用失败不计入 `ConsecFails`）

### 2.3 已有 `WindowCostStickyOnly` 三态机制（account.go:1444-1454）

```go
type WindowCostSchedulability int
const (
    WindowCostSchedulable     // 可正常调度
    WindowCostStickyOnly      // 仅允许粘性会话
    WindowCostNotSchedulable  // 完全不可调度
)
```

调度过滤函数已带 `isSticky bool` 参数（`isAccountSchedulableForWindowCost`、`isAccountSchedulableForRPM`）。本方案将**复用**此架构，新增 `isAccountSchedulableForHealth` 即可。

## 3. 设计方案

### 3.1 总体架构

```
请求到达
  │
  ▼
┌─────────────────────────────────────┐
│ Layer 1: 模型路由（保持现状字典序）  │  ← 不改
└─────────────────────────────────────┘
  │
  ▼
┌─────────────────────────────────────┐
│ Layer 1.5: Sticky Session           │  ← 仅加 isAccountSchedulableForHealth(_, true)
│  Redis 命中 + 健康检查（StickyOnly  │
│  允许、Excluded 拒绝）              │
└─────────────────────────────────────┘
  │未命中
  ▼
┌─────────────────────────────────────┐
│ Layer 2: 算法分流（新增）            │
│                                      │
│  algorithm=legacy   → 现有字典序     │  ← 保留
│  algorithm=weighted → 加权打分       │  ← 新增
│                                      │
└─────────────────────────────────────┘
  │
  ▼
SetSessionAccountID（写入 sticky）
```

### 3.2 数据模型：Bucketed Rolling Window

替换现有 `AccountTestHealth` 中分散字段为统一的环形桶结构。

```go
const (
    healthBucketCount    = 10      // 10 个桶
    healthBucketSeconds  = 60      // 每桶 1 分钟
    healthWindowSeconds  = healthBucketCount * healthBucketSeconds // 10 分钟
)

type metricBucket struct {
    startSec  int64    // 桶起点 unix 秒；0 表示空桶
    reqCount  int      // 总请求
    errCount  int      // 失败次数
    ttftSum   int64    // TTFT 累加（ms）
    ttftN     int      // TTFT 样本数
    otpsSum   float64  // OTPS 累加
    otpsN     int      // OTPS 样本数
    slowCount int      // 慢请求数（duration ≥ slowThresholdMs）
}

type AccountTestHealth struct {
    mu      sync.Mutex
    buckets [healthBucketCount]metricBucket
    cursor  int            // 当前写入桶下标

    // test 退避状态机（保留，不窗口化）
    ConsecFails         int
    RetryInterval       time.Duration
    NextRetryAt         time.Time
    TempUnschedDuration time.Duration
    LastStatus          string
    LastTestedAt        time.Time

    // 状态变化追踪（用于日志去抖）
    lastVerdict HealthVerdict
}
```

**写入入口**统一为 `Record(sample CallSample)`：

```go
type CallSample struct {
    Success      bool
    TTFTMs       int    // 0 表示无 TTFT 样本
    DurationMs   int    // 总耗时
    OutputTokens int    // 0 表示无 OTPS 样本
}

func (c *AccountTestHealthCache) Record(accountID int64, sample CallSample) {
    h := c.GetOrCreate(accountID)
    h.mu.Lock()
    defer h.mu.Unlock()
    b := h.advanceBucket(time.Now())   // 必要时滚动到新桶（清空过期桶）
    b.reqCount++
    if !sample.Success { b.errCount++ }
    if sample.TTFTMs > 0 { b.ttftSum += int64(sample.TTFTMs); b.ttftN++ }
    if sample.OutputTokens >= 10 && sample.TTFTMs > 0 && sample.DurationMs > sample.TTFTMs {
        decodeMs := sample.DurationMs - sample.TTFTMs
        otps := float64(sample.OutputTokens-1) * 1000.0 / float64(decodeMs)
        b.otpsSum += otps; b.otpsN++
    }
    if sample.DurationMs >= c.cfg.slowThresholdMs { b.slowCount++ }
}
```

**读取入口**统一为 `Snapshot()`：

```go
type HealthSnapshot struct {
    ReqCount  int
    ErrCount  int
    ErrRate   float64
    TTFTAvg   float64
    OTPSAvg   float64
    SlowRate  float64
    HasTTFT   bool
    HasOTPS   bool
}

func (h *AccountTestHealth) snapshot(now time.Time, windowSec int64) HealthSnapshot {
    var s HealthSnapshot
    cutoff := now.Unix() - windowSec
    var ttftSum int64
    var otpsSum float64
    var ttftN, otpsN int
    for i := range h.buckets {
        b := &h.buckets[i]
        if b.startSec == 0 || b.startSec < cutoff { continue }
        s.ReqCount += b.reqCount
        s.ErrCount += b.errCount
        ttftSum += b.ttftSum; ttftN += b.ttftN
        otpsSum += b.otpsSum; otpsN += b.otpsN
        // slowCount 累加略
    }
    if s.ReqCount > 0 { s.ErrRate = float64(s.ErrCount) / float64(s.ReqCount) }
    if ttftN > 0 { s.TTFTAvg = float64(ttftSum) / float64(ttftN); s.HasTTFT = true }
    if otpsN > 0 { s.OTPSAvg = otpsSum / float64(otpsN); s.HasOTPS = true }
    return s
}
```

### 3.3 HealthVerdict 三态判定

```go
type HealthVerdict int
const (
    HealthOK HealthVerdict = iota
    HealthStickyOnly  // 已粘的 session 继续，新 session 避开
    HealthExcluded    // 全员剔除
)

func (c *AccountTestHealthCache) HealthVerdict(accountID int64) HealthVerdict {
    h := c.Get(accountID)
    if h == nil { return HealthOK }
    h.mu.Lock(); defer h.mu.Unlock()
    s := h.snapshot(time.Now(), c.cfg.windowSec)

    if s.ReqCount < c.cfg.minSamples { return HealthOK }  // 样本不足→中性

    // 硬阈值：彻底排除（含 sticky）
    if s.ErrCount >= c.cfg.errCountHard ||
       s.ErrRate >= c.cfg.errRateHard {
        return HealthExcluded
    }

    // 软阈值：仅粘性
    if s.ErrCount >= c.cfg.errCountSoft ||
       s.ErrRate >= c.cfg.errRateSoft ||
       (s.HasTTFT && s.TTFTAvg >= float64(c.cfg.ttftStickyOnlyMs)) ||
       (s.HasOTPS && s.OTPSAvg < c.cfg.otpsStickyOnlyMin) {
        return HealthStickyOnly
    }

    return HealthOK
}
```

**Gateway 层包装**（沿用现有签名风格）：

```go
func (s *GatewayService) isAccountSchedulableForHealth(account *Account, isSticky bool) bool {
    if account.Platform != PlatformAnthropic || s.healthCache == nil { return true }
    switch s.healthCache.HealthVerdict(account.ID) {
    case HealthOK:         return true
    case HealthStickyOnly: return isSticky
    case HealthExcluded:   return false
    }
    return true
}
```

调用点：
- Layer 1.5 粘性会话路径：`isAccountSchedulableForHealth(account, true)`
- Layer 2 新会话路径：`isAccountSchedulableForHealth(acc, false)`

### 3.4 加权打分公式

候选硬过滤通过后，每个账号计算 5 因子归一化（[0,1]，1=好），加权求和。

#### 3.4.1 因子归一化

```go
// 价格（priority 数字小=便宜=好）：候选池相对归一化
priorityFactor = clamp01(1 - (P - Pmin) / max(1, Pmax - Pmin))

// TTFT：绝对阈值（不依赖候选池）
//   ≤ ttft_best_ms = 1.0；≥ ttft_worst_ms = 0；线性插值
ttftFactor = clamp01((ttft_worst_ms - TTFTAvg) / (ttft_worst_ms - ttft_best_ms))

// OTPS：绝对阈值
//   ≥ otps_best = 1.0；≤ otps_worst = 0
otpsFactor = clamp01((OTPSAvg - otps_worst) / (otps_best - otps_worst))

// 错误率
errFactor = clamp01(1 - ErrRate)

// 负载（非线性，仅接近满载惩罚）
//   < load_threshold_pct = 1.0
//   load_threshold_pct ~ 100 = 线性降到 0
loadFactor = ...

// 样本数门槛：< min_samples 时中性化（不让新账号被 1 次抖动判死）
if ReqCount < min_samples {
    ttftFactor, otpsFactor, errFactor = 0.5, 0.5, 0.7
}
```

#### 3.4.2 加权求和

```go
score = w.errRate   * errFactor
      + w.ttft      * ttftFactor
      + w.otps      * otpsFactor
      + w.priority  * priorityFactor
      + w.load      * loadFactor
```

**默认权重**（合计 4.5）：

| 因子 | 权重 | 占比 | 角色 |
|---|:---:|:---:|---|
| `err_rate` | **1.5** | 33% | 可用性，最重要 |
| `ttft` | **1.2** | 27% | 用户首字感知 |
| `otps` | **1.0** | 22% | 流式吞吐感知 |
| `priority` | **0.5** | 11% | 价格软偏好（不主导）|
| `load` | **0.3** | 7% | 仅防溢出 |

性能因子 vs 价格因子比例 ≈ **3.7 : 0.5 ≈ 7:1**，"性能优先、价格次之"。

#### 3.4.3 Top-K 加权随机

防止热点：

```go
ranked := selectTopK(candidates, K)             // K 默认 5
minScore := minOf(ranked.score)
weights := []float64{}
for _, c := range ranked {
    weights = append(weights, (c.score - minScore) + 0.5)  // base 0.5 留兜底概率
}
selected := weightedRandomPick(ranked, weights)
```

种子来自 `FNV(sessionHash + previousResponseID + requestedModel + groupID)`，保证同会话稳定命中（避免抖动）；无会话锚点时混入时间熵（避免长期固定）。

### 3.5 错误数硬过滤

错误数（绝对值）作为**额外止损**，独立于错误率：

```go
if reqCount >= 5 && (errCount >= errCountHard || errRate >= errRateHard) {
    return excluded  // 在 HealthVerdict 内返回 HealthExcluded
}
```

不进入打分，不重复发力。

### 3.6 完整调度链（Layer 2 weighted 算法）

```go
func (s *GatewayService) selectByWeightedScore(...) (*AccountSelectionResult, error) {
    // 1. 候选硬过滤（沿用现有 isAccountSchedulableFor* 系列 + 新增 Health）
    candidates := []
    for _, acc := range accounts {
        if !s.isAccountSchedulableForSelection(acc) { continue }
        if !s.isAccountAllowedForPlatform(acc, platform, useMixed) { continue }
        if !s.isModelSupportedByAccountWithContext(ctx, acc, model) { continue }
        if !s.isAccountSchedulableForQuota(acc) { continue }
        if !s.isAccountSchedulableForWindowCost(ctx, acc, false) { continue }
        if !s.isAccountSchedulableForRPM(ctx, acc, false) { continue }
        if !s.isAccountSchedulableForHealth(acc, false) { continue } // 新增
        if isExcluded(acc.ID) { continue }
        candidates = append(candidates, acc)
    }

    // 2. 拿负载、健康快照，算分
    loadMap := s.concurrencyService.GetAccountsLoadBatch(...)
    scored := []
    for _, acc := range candidates {
        if loadMap[acc.ID].LoadRate >= 100 { continue }
        snapshot := s.healthCache.Snapshot(acc.ID)
        score, factors := computeScore(acc, loadMap[acc.ID], snapshot, weights, thresholds, minSamples)
        scored = append(scored, scoredCandidate{acc, snapshot, loadMap[acc.ID], score, factors})
    }
    if len(scored) == 0 { return nil, ErrNoAvailableAccounts }

    // 3. Top-K 加权随机
    ranked := selectTopK(scored, topK)
    seed := deriveSelectionSeed(sessionHash, requestedModel, groupID)
    order := buildWeightedOrder(ranked, seed)

    // 4. 抢槽（失败回到 order 下一个）
    for _, c := range order {
        result, err := s.tryAcquireAccountSlot(ctx, c.account.ID, c.account.Concurrency)
        if err == nil && result.Acquired {
            if !s.checkAndRegisterSession(ctx, c.account, sessionHash) {
                result.ReleaseFunc(); continue
            }
            s.cache.SetSessionAccountID(ctx, derefGroupID(groupID), sessionHash, c.account.ID, stickySessionTTL)
            return s.newSelectionResult(ctx, c.account, true, result.ReleaseFunc, nil)
        }
    }

    // 5. 全部抢槽失败 → 兜底排队（沿用现有 Layer 3）
    return s.fallbackWaitPlan(...)
}
```

## 4. 配置

### 4.1 配置结构（`config.GatewaySchedulingConfig` 扩展）

```yaml
gateway:
  scheduling:
    # === 现有字段保留 ===
    sticky_session_max_waiting: 3
    sticky_session_wait_timeout: 45s
    fallback_wait_timeout: 30s
    fallback_max_waiting: 100
    load_batch_enabled: true
    slot_cleanup_interval: 30s
    fallback_selection_mode: last_used

    # === 阶段 0：priority 容差 ===
    priority_tolerance: 5            # filterByMinPriority 容差，默认 5（倍率差 0.05）

    # === 阶段 1+：算法切换 ===
    algorithm: legacy                # legacy | weighted
    weighted_groups: []              # 强制走 weighted 的 group ID 白名单
    legacy_groups: []                # 强制走 legacy 的 group ID 白名单（最高优先级）

    # === 阶段 2：健康判定阈值 ===
    health:
      window_minutes: 10
      min_samples: 5
      err_count_soft: 5              # → HealthStickyOnly
      err_count_hard: 10             # → HealthExcluded
      err_rate_soft: 0.3
      err_rate_hard: 0.5
      ttft_sticky_only_ms: 8000
      otps_sticky_only_min: 10

    # === 阶段 2：加权打分 ===
    score_weights:
      err_rate: 1.5
      ttft: 1.2
      otps: 1.0
      load: 0.3
      priority: 0.5
    score_thresholds:
      ttft_best_ms: 1500
      ttft_worst_ms: 6000
      otps_best: 80
      otps_worst: 10
      load_threshold_pct: 70
    top_k: 5

    # === 阶段 2：调试观测 ===
    debug:
      log_decisions: false
      log_groups: []                 # 仅这些 group 打详细日志
      log_sample_rate: 0.05
      log_score_details: false
      compare_mode: false            # 比对模式：同时跑 legacy + weighted，记录 diff
```

### 4.2 算法选择优先级

```
LegacyGroups（豁免）  >  WeightedGroups（白名单）  >  全局 algorithm 字段
```

```go
func (s *GatewayService) chooseAlgorithm(groupID int64) string {
    cfg := s.cfg.Gateway.Scheduling
    for _, gid := range cfg.LegacyGroups {
        if gid == groupID { return "legacy" }
    }
    for _, gid := range cfg.WeightedGroups {
        if gid == groupID { return "weighted" }
    }
    if cfg.Algorithm == "weighted" { return "weighted" }
    return "legacy"
}
```

## 5. 日志与 Metrics

### 5.1 决策日志（采样）

```json
{
  "msg": "scheduler_decision",
  "group_id": 1,
  "session": "8a3f...",
  "model": "claude-sonnet-4",
  "algorithm": "weighted",
  "layer": "score",
  "candidate_total": 12,
  "candidate_excluded": 2,
  "candidate_sticky_only": 1,
  "candidate_load_full": 0,
  "top_k": 5,
  "selected_account": 7,
  "selected_score": 3.42,
  "selected_priority": 10,
  "duration_us": 1240
}
```

采样规则：
- 默认 5%（`log_sample_rate: 0.05`）
- `log_groups` 白名单内强制 100%
- 全部决策都进 metrics（不抽样）

### 5.2 候选明细（debug 模式）

仅在 `log_score_details: true` 或采样命中时打。

```json
{
  "msg": "scheduler_candidate",
  "account_id": 7,
  "priority": 10,
  "score": 3.42,
  "f_err": 0.99, "f_ttft": 0.93, "f_otps": 0.86, "f_load": 1.0, "f_priority": 1.0,
  "raw_err_rate": 0.01, "raw_ttft_ms": 1850, "raw_otps": 72, "raw_load": 35,
  "verdict": "OK",
  "selected": true
}
```

### 5.3 状态变化日志（去抖）

仅在 `HealthVerdict` 切换瞬间打：

```json
{
  "msg": "account_health_state_change",
  "account_id": 7,
  "from": "OK",
  "to": "StickyOnly",
  "trigger": "err_rate_soft",
  "window_req": 50,
  "window_err": 16,
  "window_err_rate": 0.32,
  "window_ttft_avg_ms": 4200
}
```

实现：在 `AccountTestHealth` 加 `lastVerdict` 字段，每次 `HealthVerdict()` 对比并打日志。

### 5.4 告警日志

```json
// 候选全军覆没
{
  "level": "WARN",
  "msg": "no_healthy_account",
  "group_id": 1,
  "model": "claude-sonnet-4",
  "candidate_total": 5,
  "all_excluded": ["7:err_count_hard", "8:err_rate_hard"]
}

// 流量集中（后台周期检测）
{
  "level": "WARN",
  "msg": "scheduler_hotspot_detected",
  "account_id": 7,
  "qps": 45,
  "fleet_avg_qps": 8,
  "concentration_ratio": 5.6
}
```

### 5.5 Metrics 列表

接入 `ops_metrics_collector.go`：

| Metric | 类型 | Labels | 用途 |
|---|---|---|---|
| `scheduler.algorithm.requests` | Counter | algorithm, layer, group | 算法分流统计 |
| `scheduler.score.histogram` | Histogram | group | score 分布，调参依据 |
| `scheduler.candidate.filtered` | Counter | reason | 过滤原因分布 |
| `scheduler.selected.priority` | Histogram | group | 选中账号 priority 分布（验证性能优先）|
| `scheduler.selected.account` | Counter | account_id, group | 流量集中度 |
| `scheduler.selection.duration_us` | Histogram | algorithm | 调度耗时 |
| `scheduler.health.verdict` | Gauge | account_id, verdict | 实时健康状态 |
| `scheduler.account.err_rate` | Gauge | account_id | 滑动窗口错误率 |
| `scheduler.account.ttft_avg_ms` | Gauge | account_id | 滑动窗口 TTFT |
| `scheduler.account.otps_avg` | Gauge | account_id | 滑动窗口 OTPS |
| `scheduler.account.window_req_count` | Gauge | account_id | 窗口样本数 |

## 6. Admin 调试接口

### 6.1 实时快照

```
GET /admin/scheduler/snapshot?group_id=1
```

```json
{
  "algorithm": "weighted",
  "candidates": [
    {
      "account_id": 7, "priority": 10, "verdict": "OK",
      "window": {
        "req_count": 50, "err_count": 1, "err_rate": 0.02,
        "ttft_avg_ms": 1850, "otps_avg": 72, "slow_rate": 0.04
      },
      "load_rate": 35, "score": 3.42,
      "factors": {"err": 0.99, "ttft": 0.93, "otps": 0.86, "load": 1.0, "priority": 1.0}
    }
  ]
}
```

### 6.2 干跑（dry-run）

```
POST /admin/scheduler/dry-run
{
  "group_id": 1,
  "model": "claude-sonnet-4",
  "weights": {"err_rate": 1.5, ...}    // 可选：临时覆盖权重看效果
}
```

返回：会选谁、各候选 score、不实际占用槽位。**调权重前必备**。

### 6.3 比对模式

`debug.compare_mode: true` 时，weighted 路径里同时跑一遍 legacy：

```go
if cfg.Debug.CompareMode {
    legacyChoice := s.selectByLegacyLayered(ctx, ...)
    weightedChoice := s.selectByWeightedScore(ctx, ...)
    if legacyChoice.account.ID != weightedChoice.account.ID {
        logger.Info("scheduler_algorithm_diff",
            "legacy_selected", legacyChoice.account.ID,
            "weighted_selected", weightedChoice.account.ID,
            ...)
    }
    return weightedChoice
}
```

跑一周统计：
- 不一致比例（30–60% 才有意义）
- 不一致时 weighted 选的账号事后真实表现是否更好

## 7. 实施阶段

### 7.1 阶段 0：priority 容差（**0.5 天，零风险**）

**改动**：`filterByMinPriority` 加 `tolerance` 参数；config 加 `PriorityTolerance`；调用点更新。

**预期**：立即缓解"价格独裁" 70%，便宜档之间的账号能进入 LoadRate/LatencyBucket/SlowBucket 评估。

**回滚**：配置 `priority_tolerance: 0` 即恢复 `==` 严格语义。

### 7.2 阶段 1：基础设施（**1.5 天**）

**1.1 配置骨架**：
- config.go 加 `algorithm`、`weighted_groups`、`legacy_groups`、`health`、`score_weights` 等字段
- gateway_service.go 加 `chooseAlgorithm()` 路由
- Layer 2 入口 `selectByLayer2()` 分流（暂时 weighted 直接退回 legacy）

**1.2 HealthVerdict + 真实调用接入**：
- account_test_health_cache.go 加 `HealthVerdict()`、`ReportRealCall()`（先用现有 `ConsecFails` 字段实现）
- gateway_service.go 加 `isAccountSchedulableForHealth()`
- gateway_handler.go 加真实调用成败上报点
- Layer 1.5 + Layer 2 加 health 检查调用

**预期**：错误率信号融合，HealthStickyOnly 概念生效（性能差账号不接新会话）。

**回滚**：配置不引入 `weighted_groups`，新增检查全部走默认 `HealthOK`。

### 7.3 阶段 2：核心重构（**2-3 天**）

**2.1 滑动窗口重写**：
- account_test_health_cache.go 数据结构重写为 bucketed rolling window
- Record() 统一写入入口，替代 UpdateTTFT/UpdateDuration/UpdateFromTest
- Snapshot() 派生指标
- 保持 LatencyBucket/SlowBucket 向后兼容（基于新窗口实现）

**2.2 加权打分实现**：
- gateway_service.go 加 `computeAccountScore()`、`selectTopK()`、`buildWeightedOrder()`
- `selectByWeightedScore()` 完整实现
- `selectByLegacyLayered()` 保留为现有代码

**2.3 日志/Metrics/Admin**：
- 决策日志、状态变化日志、告警
- ops_metrics_collector 接入
- /admin/scheduler/snapshot、/admin/scheduler/dry-run
- 比对模式

**回滚**：配置 `algorithm: legacy` 即可。Legacy 代码完整保留。

### 7.4 阶段 3：OTPS 接入（**1 天，可选**）

- gateway_handler.go 提取 output_tokens 并上报
- Record() 中按业内公式计算 OTPS（已在 3.2 设计）
- 验证 OTPS 因子在打分中实际生效

**回滚**：配置权重 `otps: 0`。

### 7.5 阶段 4：测试与灰度（**滚动进行**）

测试单元覆盖：
- account_test_health_cache_test.go：bucket 滚动、HealthVerdict、Record、并发安全
- scheduler_score_test.go（新增）：打分公式、Top-K、加权随机种子稳定性
- scheduler_layered_filter_test.go：保留 legacy 测试，新增 weighted 路径用例

灰度路径：
1. **本地测试 group**：`weighted_groups: [测试 group]`，比对模式开启，跑 1-2 天
2. **小流量**：扩到 10-20% group，观察 metrics 24-48h
3. **核心 group**：扩到 50%，观察 1 周
4. **全量**：`algorithm: weighted`，`legacy_groups: []`
5. **清理**：全量稳定运行 2 周后删除 legacy 代码

## 8. 风险与回滚

| 风险 | 影响 | 缓解 | 回滚 |
|---|---|---|---|
| 权重配错，流量集中坏账号 | 用户错误率上升 | 比对模式 + 灰度 + dry-run 调参 | 改 weights 配置 |
| 阈值过严，HealthStickyOnly 覆盖过广 | 频繁切换账号、用户感知抖动 | metrics 监控 verdict 分布 | 调高阈值或全 OK |
| 滑动窗口锁竞争 | 调度延迟增加 | 锁内只算 O(N=10)；benchmark | 回退 legacy |
| 真实调用失败语义不准 | 误报错误率 | 严格定义 success/fail（详 §10）| 关闭上报 |
| 重启丢失窗口数据 | 重启后短暂全部 HealthOK | 接受（同 EWMA 现状）| 无 |
| 日志量爆炸 | 磁盘/转发压力 | 5% 采样默认；白名单全量 | log_sample_rate=0 |

**热路径回滚单元**：
- 配置开关：秒级（改 yaml 重启或 hot reload）
- 算法回退：全局改 `algorithm: legacy`
- 健康检查回退：全局改 `health.min_samples: 999999`（永远不过门槛）
- 数据结构回退：需重启（接受）

## 9. 测试方案

### 9.1 单元测试

- `account_test_health_cache_test.go`
  - bucket 滚动正确性（边界、跨小时）
  - Record() 各字段累加
  - Snapshot() 在窗口内/外/部分过期下的正确性
  - HealthVerdict() 三态切换
  - 并发安全（go test -race）

- `scheduler_score_test.go`（新增）
  - 因子归一化边界（min=max、空池、单候选）
  - 权重组合
  - Top-K 选择
  - 加权随机种子稳定性（同 sessionHash 多次调用结果一致）
  - 价格主导验证（性能差时贵账号能赢）

- `scheduler_layered_filter_test.go`
  - 保留所有 legacy 用例
  - 新增 weighted 路径完整链路用例

### 9.2 集成测试

- 端到端：构造 5 个账号（不同 priority、TTFT、err_rate）→ 调度 1000 次 → 统计选中分布是否符合权重比例
- 灰度切换：weighted_groups 变更后立即生效（无需重启）
- 比对模式：legacy/weighted 同时跑不影响调度结果

### 9.3 性能验证

- benchmark：weighted 算法单次调度 < 5ms（候选 50 账号）
- 锁竞争：snapshot 调用 1000 QPS 下 p99 < 1ms

## 10. 失败语义定义

`gateway_handler.go` 上报真实调用 `Success` 字段，语义与 ops `error_owner` 分类对齐：**只有 `error_owner = 'provider'` 的情况才计为失败**，客户端请求问题不应影响账号健康评分。

| 场景 | success | 说明 |
|---|:---:|---|
| HTTP 2xx 且流正常结束（`message_stop`）| ✅ true | — |
| HTTP 2xx 但流中途 error event | ❌ false | `UpstreamFailoverError{StatusCode:403}` |
| HTTP 401 / 403 认证/授权失败 | ❌ false | 账号凭证问题，`error_owner='provider'` |
| HTTP 429 rate limited（上游限流）| ❌ false | 账号被限速，`error_owner='provider'` |
| HTTP 5xx 服务端错误 | ❌ false | 上游不可用，`error_owner='provider'` |
| 网络 / 连接错误 | ❌ false | 传输层失败，`error_owner='provider'` |
| 响应头超时（`context.DeadlineExceeded`）| ❌ false | 上游响应过慢，`error_owner='provider'` |
| HTTP 400（服务端临时限流，isRetryLater400）| ❌ false | 上游瞬时限流，返回 `UpstreamFailoverError` |
| HTTP 400 `invalid_request_error`（用户请求格式问题）| ⚠️ **不上报** | `ClientRequestError`，`error_owner='client'` |
| `BetaBlockedError`（不支持的 Beta 特性）| ⚠️ **不上报** | 用户请求内容问题，`error_owner='client'` |
| 客户端主动中断（`context.Canceled`）| ⚠️ **不上报** | 非账号问题 |
| 流正常但客户端中途断开（`ClientDisconnect`）| ⚠️ **不上报** | 非账号问题 |

**实现要点**（`reportAnthropicForwardResult`）：
1. 平台检查：非 `PlatformAnthropic` 直接返回
2. 排除 `BetaBlockedError`（`errors.As`）
3. 排除 `ClientRequestError`（`errors.As`）——`handleErrorResponse` 对 400 `invalid_request_error` 返回此类型
4. 排除 `context.Canceled`（`errors.Is`）
5. 排除 `result.ClientDisconnect`
6. 其余 `err != nil` 均视为 `Success=false` 写入滑动窗口

## 11. 调参指南

上线后看 metrics dashboard 调整：

| 现象 | 调整 |
|---|---|
| `score.histogram` 中位数 < 2.0 | 阈值过严，降 `ttft_worst_ms` / `otps_worst` 或调阈值 |
| `selected.priority` 集中头部（90%+ 都是 priority=10）| 性能权重不够，加 `err_rate/ttft/otps` 各 0.2 |
| `selected.account` 单账号 QPS > 平均 3 倍 | 热点，加大 `top_k` 或 weight base（0.5 → 1.0）|
| `compare_mode` diff 率 < 20% | 等于没改，权重要拉大 |
| `compare_mode` diff 率 > 70% | 改动过激，权重收敛 |
| `verdict` 长期 StickyOnly 占比 > 30% | 软阈值过严，调高 `err_rate_soft` 等 |
| `no_healthy_account` 频发 | 硬阈值过严或样本量不够 |

## 12. 术语

- **Priority（账号字段）**：账号的优先级数字，越小越优先。本系统中由用户按倍率正相关设置（0.10 → 10）。
- **`weights.priority`（评分权重）**：加权打分中的全局系数，与 Priority 无关。
- **TTFT**：Time To First Token，首字延时（ms）。
- **OTPS**：Output Tokens Per Second，输出 token 速率。业内公式 `(output_tokens-1) * 1000 / (duration - TTFT)`。
- **Sticky session**：sessionHash → accountID 的 Redis 绑定，TTL 1h，命中时刷新。
- **HealthOK / HealthStickyOnly / HealthExcluded**：本方案三态健康判定。复用现有 `WindowCostStickyOnly` 架构。
- **Bucketed rolling window**：N 个时间桶环形缓冲，支持精确"过去 N 分钟"语义聚合。

## 13. 关联文档

- `docs/anthropic-account-scheduling.md`：现有调度链文档
- `docs/anthropic-scheduling-health-aware.md`：健康感知调度（已实现部分）
- `docs/upstream-error-threshold.md`：上游错误阈值

---

**附**：本方案改动行数估算

| 类别 | 行数 | 备注 |
|---|:---:|---|
| 生产代码净增 | ~700 | 含配置、健康缓存重写、打分、admin |
| 测试代码 | ~700 | 重写 + 新增 |
| 文档 | ~300 | 本文档 + 注释 |
| **总计** | **~1700** | 1 名熟悉代码工程师 3-5 工作日 |
