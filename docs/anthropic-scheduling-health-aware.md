# Anthropic 调度健康感知方案（当前实现）

## 背景

Anthropic 账号调度需要感知供应商的健康状态，避免将流量持续路由到延迟高、错误率高或不可用的账号。

本方案包含两层能力：

- **账号级 HealthVerdict 三态**：基于账号 10min 滑动窗口指标（错误率、TTFT、OTPS）限制新会话或临时排除账号。
- **模型级质量分桶**：基于 `(account_id, requested_model)` 维度的 TTFT / OTPS / CacheHit 滑动窗口，在 Layer 2 选号时优先选择质量分桶更好的账号。

## 整体架构

```
┌──────────────────────────────────────────────────────────────────┐
│  HealthVerdict 三态（账号级内存滑动窗口）                              │
│  基于 10min 滑动窗口的错误率/TTFT/OTPS 判定 OK/StickyOnly/Excluded │
│  需 account_health.enabled=true 才生效                             │
├──────────────────────────────────────────────────────────────────┤
│  TempUnschedulable（DB，现有机制）                                  │
│  账号级硬不可调度，粘性会话由 shouldClearStickySession 自动清除      │
│  由 HealthExcluded 首次切换时异步触发                               │
└──────────────────────────────────────────────────────────────────┘
┌──────────────────────────────────────────────────────────────────┐
│  AccountModelQualityCache（account+model 维度内存滑动窗口）          │
│  TTFT bucket → OTPS bucket → CacheHit bucket                      │
│  仅在 account_health.enabled=true 时参与 Layer 2 排序              │
└──────────────────────────────────────────────────────────────────┘
```

## 核心数据结构

### AccountTestHealth（进程内内存，不持久化）

```go
type AccountTestHealth struct {
    // 滑动窗口（环形 buffer，10 个桶 × 1min/桶 = 10min 窗口）
    buckets [10]metricBucket
    cursor  int

    LastStatus   string
    LastTestedAt time.Time
}

// 每个时间桶记录 1 分钟内的累计指标
type metricBucket struct {
    startSec        int64
    reqCount        int
    errCount        int
    slowCount       int
    ttftSum         int64   // ms
    ttftN           int
    otpsSum         float64 // tokens/s
    otpsN           int
    tcpConnSum      int64   // ms
    tcpConnN        int
    ttfbSum         int64   // ms
    ttfbN           int
    cacheHitRateSum float64
    cacheHitN       int
}
```

存储：`sync.Map`，key 为 `accountID int64`，进程内单例。进程重启后缓存为空，所有账号视为"无数据"，不惩罚。

### CallSample（统一上报结构）

```go
type CallSample struct {
    Success             bool
    TTFTMs              int // 首 token 延迟（ms），0 表示无样本
    DurationMs          int // 请求总时长（ms），0 表示无样本
    OutputTokens        int // 输出 token 数，0 表示无样本
    TCPConnMs           int // TCP 连接时间（ms），0 表示无样本
    TTFBMs              int // 首字节时间（ms），0 表示无样本
    CacheReadTokens     int // Anthropic cache_read_input_tokens
    CacheCreationTokens int // Anthropic cache_creation_input_tokens
    InputTokens         int // Anthropic input_tokens（不含 cache 部分）
}
```

主动 test 和真实 gateway 调用共用同一份账号级滑动窗口（融合视图）。真实 gateway 调用还会按 `account+model` 写入模型质量缓存，用于 Layer 2 分桶排序。

### HealthSnapshot（窗口聚合快照）

```go
type HealthSnapshot struct {
    ReqCount           int
    ErrCount           int
    SlowCount          int
    TTFTSampleCount    int
    OTPSSampleCount    int
    TCPConnSampleCount int
    TTFBSampleCount    int
    CacheHitSampleCount int
    // 内部字段：ttftSumMs, otpsSum, tcpConnSumMs, ttfbSumMs, cacheHitRateSum
}

// 派生指标
ErrRate()         float64 // ErrCount / ReqCount
SlowRate()        float64 // SlowCount / ReqCount
TTFTAvg()         float64 // ttftSumMs / TTFTSampleCount（ms）
OTPSAvg()         float64 // otpsSum / OTPSSampleCount（tokens/s）
TCPConnAvg()      float64 // tcpConnSumMs / TCPConnSampleCount（ms）
TTFBAvg()         float64 // ttfbSumMs / TTFBSampleCount（ms）
CacheHitRateAvg() float64 // cache_read / (cache_read + cache_creation + input)
```

### OTPS 计算公式

```
OTPS = (output_tokens - 1) * 1000 / (duration_ms - ttft_ms)
```

过滤条件：`output_tokens >= 10` 且 `duration_ms > ttft_ms > 0`，避免除零或负值。

---

## 模型级质量分桶

`AccountModelQualityCache` 按 `(account_id, requested_model)` 存储 TTFT / OTPS / CacheHit 指标，只用于 Layer 2 选号，不做 HealthVerdict 判定，也不触发 TempUnschedulable。

写入入口为真实请求完成后的 `RecordAnthropicCall(accountID, requestedModel, sample)`。其中 `requestedModel` 使用调度时的原始模型（渠道映射前），保证写入 key 与调度读取 key 一致。

Layer 2 当前排序链为：

```
Priority → TTFT bucket → OTPS bucket → CacheHit bucket → LoadRate → LRU
```

分桶规则如下，编号越小越优先：

| 指标 | 分桶规则 | 冷启动 |
|------|----------|--------|
| TTFT | `<4000ms → 0`；`4000ms` 起每 `3000ms` 增加一档 | 无样本视为 0 |
| OTPS | `>=60 → 0`；`40-60 → 1`；`20-40 → 2`；`<20 → 3` | 无样本视为 0 |
| CacheHit | `>=0.80 → 0`；`0.60-0.80 → 1`；`0.40-0.60 → 2`；`0.20-0.40 → 3`；`<0.20 → 4` | 无样本视为 0 |

`account_health.enabled=false` 时 `qualityBucketCache()` 返回 `nil`，三个分桶过滤函数直接透传候选集，Layer 2 退回 `Priority → LoadRate → LRU`。

---

## HealthVerdict 三态

需要 `gateway.scheduling.account_health.enabled=true` 才生效（默认 `false`）。

配置路径：`gateway.scheduling.account_health.enabled`

### 判定逻辑

基于 10min 滑动窗口快照，按优先级依次判断：

```
样本数 < MinSamples（=5）→ HealthOK（冷启动保护）

ErrCount >= ErrCountHard（=5）→ HealthExcluded
ErrRate  >= ErrRateHard（=0.5）→ HealthExcluded
TTFTAvg  >= TTFTExcludedMs（默认 0，禁用）→ HealthExcluded
OTPSAvg  <  OTPSExcludedMin（默认 0，禁用）→ HealthExcluded

ErrCount >= ErrCountSoft（=3）  → HealthStickyOnly
ErrRate  >= ErrRateSoft（=0.3） → HealthStickyOnly
TTFTAvg  >= TTFTStickyOnlyMs（=10000ms）→ HealthStickyOnly
OTPSAvg  <  OTPSStickyOnlyMin（=20 tokens/s）→ HealthStickyOnly

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

## 数据上报入口

### 入口1：定时测试完成（`runOnePlan`）

```go
// scheduled_test_runner_service.go
s.healthCache.UpdateFromTest(plan.AccountID, result)
```

`UpdateFromTest` 写入滑动窗口（TTFT 样本，成功/失败计数），并检查 HealthVerdict 是否变化。

### 入口2：真实请求完成（`reportAnthropicForwardResult`）

```go
// gateway_handler.go，Forward 调用返回后
h.gatewayService.RecordAnthropicCall(account.ID, reqModel, sample)
```

`RecordAnthropicCall` 同时写入两处：

- `healthCache.RecordRealCall`：账号级滑动窗口，用于 HealthVerdict 三态
- `modelQualityCache.Record`：`account+model` 维度滑动窗口，用于 TTFT / OTPS / CacheHit 分桶排序

完整 `CallSample` 包含 Success、TTFTMs、DurationMs、OutputTokens、TCPConnMs、TTFBMs、CacheReadTokens、CacheCreationTokens、InputTokens。

**不上报的情况**（对应 `error_owner = 'client'` 或非账号问题，避免用户请求问题污染账号健康评分）：
- `context.Canceled`（客户端中断，非账号问题）
- `BetaBlockedError`（请求使用了不支持的 Beta 特性）
- `ClientRequestError`（上游返回 400 `invalid_request_error`，属于用户请求格式问题）
- `result.ClientDisconnect`（流正常但客户端中途断开）

> **注**：上游触发 failover 的 400（如 `isRetryLater400` 服务端临时限流）仍返回 `UpstreamFailoverError`，会正常计入失败。

---

## 涉及文件

| 文件 | 职责 |
|------|------|
| `service/account_test_health_cache.go` | 核心数据结构：环形 bucket 滑动窗口、CallSample、HealthSnapshot、HealthVerdict 三态判定、OTPS 计算 |
| `service/account_model_quality_cache.go` | account+model 维度质量指标缓存：TTFT / OTPS / CacheHit |
| `service/gateway_service_quality_bucket.go` | `filterByMinTTFTBucket`、`filterByMinOTPSBucket`、`filterByMinCacheHitBucket` 分桶过滤 |
| `service/gateway_service_scheduling.go` | `isAccountSchedulableForHealth`、`onHealthVerdictChange`、`healthVerdictConfig`、`logSchedulerSelected` 等健康调度方法 |
| `service/scheduled_test_runner_service.go` | `runOnePlan` 更新健康缓存；`tryRecoverAccount` 测试成功后自动恢复 |
| `service/gateway_service.go` | Layer 2 健康过滤调用（`isAccountSchedulableForHealth`）；Layer 1.5 `healthOK` 检查；`RecordAnthropicCall` 上报入口 |
| `handler/gateway_handler.go` | `reportAnthropicForwardResult` 上报真实请求样本 |
| `service/wire.go` / `cmd/server/wire_gen.go` | 注入 `AccountTestHealthCache` |

不新增 DB 表，不新增 Redis key 类型，不改粘性会话路径，不改 Layer 0 / Layer 1。

---

## 关键参数

### 账号健康配置（`gateway.scheduling.account_health`）

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `enabled` | false | **总开关**，false 时 HealthVerdict 三态不生效 |
| `slow_threshold_ms` | 20000ms | 慢请求判定阈值（DurationMs ≥ 此值计入 slowCount） |

### HealthVerdict 三态阈值（`gateway.scheduling.health`）

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `window_minutes` | 10 | 滑动窗口长度（分钟） |
| `min_samples` | 5 | 触发判定的最小样本数 |
| `err_count_soft` | 3 | 错误数 ≥ 此值 → StickyOnly |
| `err_count_hard` | 5 | 错误数 ≥ 此值 → Excluded |
| `err_rate_soft` | 0.3 | 错误率 ≥ 此值 → StickyOnly |
| `err_rate_hard` | 0.5 | 错误率 ≥ 此值 → Excluded |
| `ttft_sticky_only_ms` | 10000ms | TTFTAvg ≥ 此值 → StickyOnly |
| `ttft_excluded_ms` | 0 | TTFTAvg ≥ 此值 → Excluded；0 表示禁用 |
| `otps_sticky_only_min` | 20 tokens/s | OTPSAvg < 此值（且有样本）→ StickyOnly |
| `otps_excluded_min` | 0 | OTPSAvg < 此值（且有样本）→ Excluded；0 表示禁用 |
| `excluded_temp_unsched_minutes` | 30min | 进入 Excluded 时触发 TempUnschedulable 的时长 |

---

## 边界情况

| 场景 | 行为 |
|------|------|
| 账号无定时测试计划 | HealthVerdict 由真实请求数据驱动 |
| 进程重启 | 账号级健康缓存和模型级质量缓存清空；TempUnschedulable 在 DB 中持久，仍然生效 |
| 多实例部署 | 两类内存缓存各实例独立；TempUnschedulable 写 DB 所有实例共享 |
| 供应商偶发抖动（真实请求1次失败后恢复） | 只写滑动窗口，样本不足 MinSamples 时不触发判定 |
| 定时测试偶发抖动（1次失败后恢复） | 写滑动窗口，样本不足时不触发判定，对调度无影响 |
| 样本不足（冷启动） | ReqCount < MinSamples → HealthOK，冷启动保护，不惩罚新账号 |
| OTPS 样本不足（output_tokens < 10 或非流式） | HasOTPS=false → 不参与 OTPSStickyOnly 判定 |
| CacheHit 样本不足（无 token usage） | HasCacheHit=false → 分桶视为最优，不惩罚 |
| `account_health.enabled=false`（默认） | HealthVerdict 三态不生效，Layer 2 不增加健康过滤，也不启用质量分桶排序 |
