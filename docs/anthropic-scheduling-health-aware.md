# Anthropic 调度健康感知方案（当前实现）

## 背景

Anthropic 账号调度需要感知供应商的健康状态，避免将流量持续路由到延迟高、错误率高或不可用的账号。

本方案通过两个正交机制实现：

1. **退火隔离**（Layer A + Layer B）：定时测试连续失败时快速隔离账号
2. **质量感知调度**（HealthVerdict 三态 + weighted 算法）：基于滑动窗口指标（错误率、TTFT、OTPS）影响选号

## 整体架构

```
┌──────────────────────────────────────────────────────────────────┐
│  Layer A：退火测试状态机（内存）                                    │
│  定时测试驱动，感知连续失败，加速补测，达阈值触发 Layer B              │
├──────────────────────────────────────────────────────────────────┤
│  Layer B：TempUnschedulable（DB，现有机制）                        │
│  账号级硬不可调度，粘性会话由 shouldClearStickySession 自动清除      │
├──────────────────────────────────────────────────────────────────┤
│  HealthVerdict 三态（内存滑动窗口）                                 │
│  基于 10min 滑动窗口的错误率/TTFT/OTPS 判定 OK/StickyOnly/Excluded │
├──────────────────────────────────────────────────────────────────┤
│  weighted 算法（5 因子加权打分 + Top-K 加权随机）                   │
│  ErrRate + TTFT + OTPS + Load + Priority 综合打分，选最优账号       │
└──────────────────────────────────────────────────────────────────┘
```

## 核心数据结构

### AccountTestHealth（进程内内存，不持久化）

```go
type AccountTestHealth struct {
    // 滑动窗口（环形 buffer，10 个桶 × 1min/桶 = 10min 窗口）
    buckets [10]metricBucket
    cursor  int

    // 退避状态机（定时测试专用，窗口外）
    ConsecFails         int
    RetryInterval       time.Duration
    NextRetryAt         time.Time
    TempUnschedDuration time.Duration
    LastStatus          string
    LastTestedAt        time.Time
}

// 每个时间桶记录 1 分钟内的累计指标
type metricBucket struct {
    startSec  int64
    reqCount  int
    errCount  int
    slowCount int
    ttftSum   int64   // ms
    ttftN     int
    otpsSum   float64 // tokens/s
    otpsN     int
}
```

存储：`sync.Map`，key 为 `accountID int64`，进程内单例。进程重启后缓存为空，所有账号视为"无数据"，不惩罚。

### CallSample（统一上报结构）

```go
type CallSample struct {
    Success      bool
    TTFTMs       int   // 首 token 延迟（ms），0 表示无样本
    DurationMs   int   // 请求总时长（ms），0 表示无样本
    OutputTokens int   // 输出 token 数，0 表示无样本
}
```

主动 test 和真实 gateway 调用共用同一份滑动窗口（融合视图）。

### HealthSnapshot（窗口聚合快照）

```go
type HealthSnapshot struct {
    ReqCount        int
    ErrCount        int
    SlowCount       int
    TTFTSampleCount int
    OTPSSampleCount int
    // 内部字段：ttftSumMs, otpsSum
}

// 派生指标
ErrRate()  float64  // ErrCount / ReqCount
SlowRate() float64  // SlowCount / ReqCount
TTFTAvg()  float64  // ttftSumMs / TTFTSampleCount（ms）
OTPSAvg()  float64  // otpsSum / OTPSSampleCount（tokens/s）
```

### OTPS 计算公式

```
OTPS = (output_tokens - 1) * 1000 / (duration_ms - ttft_ms)
```

过滤条件：`output_tokens >= 10` 且 `duration_ms > ttft_ms > 0`，避免除零或负值。

---

## Layer A：退火测试状态机

### 状态流转

```
正常（ConsecFails=0）
  ↓ 定时测试失败
ConsecFails=1
  · 立即触发一次补测（delay=0）

ConsecFails=2（>= HardFilterThreshold=2）
  · 新会话：Layer 2 硬过滤跳过（PassesHardFilter=false）
  · 粘性会话：继续服务
  · 投退火补测（RetryInterval=15s，每次失败翻倍，上限 5min）

ConsecFails >= TempUnschedThreshold（=4）
  · 触发 SetTempUnschedulable（写 DB，初始 30min）
  · 粘性会话：下次请求 shouldClearStickySession 检测 IsSchedulable()=false，清除绑定，强制重选
  · TempUnschedulable 到期后自动触发补测（AutoRecover 路径）
    ├── 成功 → ConsecFails=0，RetryInterval 重置，恢复正常
    └── 失败 → 重新进入退火，TempUnschedDuration 翻倍（上限 60min）
```

### 退火时间线（cron=5min，测试超时=10s）

```
T+0s    cron 触发，第 1 次失败（耗时 ≤10s）
          ConsecFails=1，立即投补测

T+10s   第 1 次补测，失败（耗时 ≤10s）
          ConsecFails=2，硬过滤生效，投 15s 退火补测

T+35s   第 2 次补测，失败（耗时 ≤10s）
          ConsecFails=3，投 30s 退火补测

T+75s   第 3 次补测，失败（耗时 ≤10s）
          ConsecFails=4，触发 TempUnschedulable（30min）

从第一次失败到完全隔离：约 75 秒
```

### 补测超时

退火补测使用独立 10s 超时（与 cron 的 5min context 隔离），避免供应商挂起时阻塞：

```go
ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
result, err := s.accountTestSvc.RunTestBackground(ctx, plan.AccountID, plan.ModelID)
```

---

## Layer B：TempUnschedulable

| 操作 | 调用 |
|------|------|
| 触发 | `rateLimitSvc.SetTempUnschedulableForScheduledTest(ctx, accountID, until, reason)` |
| reason | `"定时测试连续失败：连续失败 N 次，最后状态: error"` |
| 恢复 | `rateLimitSvc.RecoverAccountAfterSuccessfulTest`（`tryRecoverAccount`） |
| 粘性清除 | `shouldClearStickySession → IsSchedulable()=false`，下次请求自动触发 |

TempUnschedulable 时长翻倍规则（`TempUnschedDuration` 字段持久化在内存中）：

```
第 1 次触发：30min
第 2 次触发：60min（上限）
```

---

## HealthVerdict 三态

需要 `gateway.scheduling.account_health.enabled=true` 才生效（默认 `false`）。

配置路径：`gateway.scheduling.account_health.enabled`

### 判定逻辑

基于 10min 滑动窗口快照，按优先级依次判断：

```
样本数 < MinSamples（=5）→ HealthOK（冷启动保护）

ErrCount >= ErrCountHard（=10）→ HealthExcluded
ErrRate  >= ErrRateHard（=0.5）→ HealthExcluded

ErrCount >= ErrCountSoft（=5）  → HealthStickyOnly
ErrRate  >= ErrRateSoft（=0.3） → HealthStickyOnly
TTFTAvg  >= TTFTStickyOnlyMs（=10000ms）→ HealthStickyOnly
OTPSAvg  <  OTPSStickyOnlyMin（=10 tokens/s）→ HealthStickyOnly

否则 → HealthOK
```

### 三态语义

| 状态 | 新会话（Layer 2） | 粘性会话（Layer 1.5） | 额外动作 |
|------|-----------------|---------------------|---------|
| `HealthOK` | 放行 | 放行 | — |
| `HealthStickyOnly` | 拒绝 | 放行 | — |
| `HealthExcluded` | 拒绝 | 拒绝 | 首次切换时异步触发 TempUnschedulable（默认 30min） |

### HealthExcluded → TempUnschedulable

`HealthExcluded` 首次触发时（状态从非 Excluded 切换到 Excluded），通过 `AccountTestHealthCache.OnVerdictChange` 回调异步调用 `SetTempUnschedulableForScheduledTest`：

- 时长由 `gateway.scheduling.health.excluded_temp_unsched_minutes` 配置，默认 30 分钟
- TempUnschedulable 写入 DB 后，`shouldClearStickySession` 检测到 `IsSchedulable()=false`，自动清除 sticky 绑定
- 30 分钟到期后账号自动恢复可调度，无需补测
- 在 TempUnschedulable 生效前（异步写入 DB 的短暂窗口），`isAccountSchedulableForHealth` 仍同步拦截 `HealthExcluded` 账号，不存在竞态漏过

### 状态变化日志

状态切换时打一次 warn 日志（去抖，每次切换打一次）：

```
[HealthVerdictChange] account_id=123 OK->StickyOnly reason="err_rate=35.0%(≥30%)" req=20 err=7 err_rate=35.0% ttft_avg=800ms otps_avg=45.2 slow_rate=5.0%
```

失败请求上报时打 warn（`gateway.anthropic_health_failure_recorded`）：
```json
{"account_id": 123, "account_name": "xxx", "error": "...", "ttft_ms": 0, "duration_ms": 5000}
```

成功请求但窗口指标达到 StickyOnly 阈值时打 warn（`gateway.anthropic_health_degraded`）：
```json
{"account_id": 123, "window_ttft_avg_ms": 11000, "window_otps_avg": 8.5, "window_err_rate": 0.1, "window_req_count": 15}
```

---

## weighted 算法（5 因子加权打分）

### 触发条件

- `gateway.scheduling.algorithm=weighted`（或账号所在 group 在 `weighted_groups` 白名单）
- 平台为 Anthropic
- `healthCache != nil`

### 打分公式

```
score = ErrRate × errFactor
      + TTFT    × ttftFactor
      + OTPS    × otpsFactor
      + Load    × loadFactor
      + Priority × priorityFactor
```

默认权重：`ErrRate=1.5, TTFT=1.2, OTPS=1.0, Load=0.3, Priority=0.5`

### 各因子归一化

**errFactor**（错误率，越低越好）：
```
errFactor = clamp01(1.0 - ErrRate())
```

**ttftFactor**（TTFT，越低越好）：
```
TTFTAvg ≤ TTFTBestMs（=1500ms）→ 1.0
TTFTAvg ≥ TTFTWorstMs（=6000ms）→ 0.0
中间：线性插值
无样本（HasTTFT=false）→ 0.5（中性）
```

**otpsFactor**（OTPS，越高越好）：
```
OTPSAvg ≥ OTPSBest（=80 tokens/s）→ 1.0
OTPSAvg ≤ OTPSWorst（=10 tokens/s）→ 0.0
中间：线性插值
无样本（HasOTPS=false）→ 0.5（中性）
```

**loadFactor**（负载率，越低越好）：
```
LoadRate < LoadThresholdPct（=70%）→ 1.0（不参与排序）
LoadRate ≥ 100%                   → 0.0
中间：线性下降
```

**priorityFactor**（优先级，候选池相对值）：
```
priorityFactor = 1.0 - (priority - minPriority) / (maxPriority - minPriority)
priority 数字越小=越便宜=越优
```

### 样本不足保护

窗口内 `ReqCount < MinSamples（=5）` 时，性能因子取中性值，避免新账号被误判：
```
ttftFactor = 0.5
otpsFactor = 0.5
errFactor  = 0.7
```

### Top-K 加权随机

1. 按 score 降序取前 K（默认 5）个候选
2. 以 `(score - minScore) + 0.5` 为权重做加权随机，生成尝试顺序
3. 依次尝试抢槽，成功则返回；全失败则 fallthrough 到 legacy 兜底

同一 sessionHash 使用确定性种子（FNV64a hash），保证同会话选号稳定；无 sessionHash 时混入时间熵。

---

## 数据上报入口

### 入口1：定时测试完成（`runOnePlan`）

```go
// scheduled_test_runner_service.go
s.healthCache.UpdateFromTest(plan.AccountID, result)
s.launchRetryIfNeeded(plan)
```

`UpdateFromTest` 同时维护：
- 滑动窗口（写入 TTFT 样本，成功/失败计数）
- 退避状态机（成功清零 ConsecFails；失败累加并计算下次补测间隔）

### 入口2：真实请求完成（`reportAnthropicForwardResult`）

```go
// gateway_handler.go，Forward 调用返回后
h.gatewayService.RecordAnthropicCall(account.ID, sample)
```

`RecordAnthropicCall` → `healthCache.RecordRealCall`，写入完整 CallSample（Success + TTFTMs + DurationMs + OutputTokens）。

**不上报的情况**（避免误报）：
- `context.Canceled`（客户端中断，非账号问题）
- `BetaBlockedError` / `PromptTooLongError`（请求内容问题）
- `result.ClientDisconnect`（流正常但客户端中途断开）

---

## 改动范围

| 文件 | 改动内容 |
|------|---------|
| `service/account_test_health_cache.go` | 核心数据结构：环形 bucket 滑动窗口、CallSample、HealthSnapshot、HealthVerdict 三态判定、OTPS 计算 |
| `service/gateway_scheduler_weighted.go` | weighted 算法：5 因子打分、Top-K 加权随机、metrics 计数器 |
| `service/scheduled_test_runner_service.go` | `runOnePlan` 更新健康缓存；`launchRetryIfNeeded` 退火补测；`triggerTempUnschedForScheduledTest` 隔离触发 |
| `service/gateway_service.go` | `isAccountSchedulableForHealth` 三态过滤；`RecordAnthropicCall` 上报入口；`tryWeightedAnthropicSelection` 调用；`resolvedSchedulingHealth/ScoreWeights/ScoreThresholds` 配置解析 |
| `handler/gateway_handler.go` | `reportAnthropicForwardResult` 上报真实请求样本 |
| `service/wire.go` / `cmd/server/wire_gen.go` | 注入 `AccountTestHealthCache` |

不新增 DB 表，不新增 Redis key 类型，不改粘性会话路径，不改 Layer 0 / Layer 1 / Layer 1.5。

---

## 关键参数

### 退避状态机（`gateway.scheduling.account_health`）

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `enabled` | false | **总开关**，false 时 HealthVerdict 三态和 PassesHardFilter 均不生效 |
| `hard_filter_threshold` | 2 | ConsecFails 达到此值，新会话硬过滤（`PassesHardFilter=false`） |
| `temp_unsched_threshold` | 4 | ConsecFails 达到此值，触发 TempUnschedulable |
| `temp_unsched_init_minutes` | 30min | TempUnschedulable 初始时长 |
| `temp_unsched_max_minutes` | 60min | TempUnschedulable 最大时长 |
| `retry_interval_step_seconds` | 15s | 退火补测间隔步长（每次失败翻倍） |
| `retry_interval_max_seconds` | 300s | 退火补测间隔上限 |
| `slow_threshold_ms` | 20000ms | 慢请求判定阈值（DurationMs ≥ 此值计入 slowCount） |

### HealthVerdict 三态阈值（`gateway.scheduling.health`）

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `window_minutes` | 10 | 滑动窗口长度（分钟） |
| `min_samples` | 5 | 触发判定的最小样本数 |
| `err_count_soft` | 5 | 错误数 ≥ 此值 → StickyOnly |
| `err_count_hard` | 10 | 错误数 ≥ 此值 → Excluded |
| `err_rate_soft` | 0.3 | 错误率 ≥ 此值 → StickyOnly |
| `err_rate_hard` | 0.5 | 错误率 ≥ 此值 → Excluded |
| `ttft_sticky_only_ms` | 10000ms | TTFTAvg ≥ 此值 → StickyOnly |
| `otps_sticky_only_min` | 10 tokens/s | OTPSAvg < 此值（且有样本）→ StickyOnly |
| `excluded_temp_unsched_minutes` | 30min | 进入 Excluded 时触发 TempUnschedulable 的时长 |

### weighted 算法（`ScoreWeightsConfig` / `ScoreThresholdsConfig`）

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `ScoreWeights.ErrRate` | 1.5 | 错误率因子权重 |
| `ScoreWeights.TTFT` | 1.2 | TTFT 因子权重 |
| `ScoreWeights.OTPS` | 1.0 | OTPS 因子权重 |
| `ScoreWeights.Load` | 0.3 | 负载率因子权重 |
| `ScoreWeights.Priority` | 0.5 | 优先级因子权重 |
| `ScoreThresholds.TTFTBestMs` | 1500ms | TTFT ≤ 此值得满分 |
| `ScoreThresholds.TTFTWorstMs` | 6000ms | TTFT ≥ 此值得 0 分 |
| `ScoreThresholds.OTPSBest` | 80 tokens/s | OTPS ≥ 此值得满分 |
| `ScoreThresholds.OTPSWorst` | 10 tokens/s | OTPS ≤ 此值得 0 分 |
| `ScoreThresholds.LoadThresholdPct` | 70% | 负载率低于此值不参与排序 |
| `TopK` | 5 | Top-K 加权随机的 K |

---

## 边界情况

| 场景 | 行为 |
|------|------|
| 账号无定时测试计划 | ConsecFails 永远为 0，不触发退火/TempUnschedulable；HealthVerdict 和 weighted 打分由真实请求数据驱动 |
| 进程重启 | 内存缓存清空；TempUnschedulable 在 DB 中持久，仍然生效 |
| 多实例部署 | 内存缓存各实例独立；TempUnschedulable 写 DB 所有实例共享 |
| 供应商偶发抖动（1次失败后恢复） | ConsecFails=1，立即补测成功，归零，对调度无任何影响 |
| 供应商快速拒绝连接（<1s） | 退火总耗时 ~35s 完成四次失败，触发 TempUnschedulable |
| 供应商挂起不响应 | 每次测试耗尽 10s 超时，退火总耗时 ~75s 触发 TempUnschedulable |
| TempUnschedulable 期间供应商恢复 | 到期补测成功，立即恢复，ConsecFails 归零 |
| TempUnschedulable 到期仍失败 | 时长翻倍重入，最大 60min |
| 样本不足（冷启动） | ReqCount < MinSamples → HealthOK；weighted 打分取中性值（0.5/0.5/0.7） |
| OTPS 样本不足（output_tokens < 10 或非流式） | HasOTPS=false → otpsFactor=0.5，不参与 OTPSStickyOnly 判定 |
| `account_health.enabled=false`（默认） | HealthVerdict 三态和 PassesHardFilter 均不生效；weighted 算法仍可独立启用 |
