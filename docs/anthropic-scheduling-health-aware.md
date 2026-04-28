# Anthropic 调度健康感知方案（完整版）

## 背景

当前 Anthropic 账号调度（Layer 2 负载感知选择）对供应商 API 账号的健康状态感知滞后：只有账号真实请求失败后才被标记为不可调度，在此之前延时变高或不稳定的供应商仍会持续被选中，影响用户体验。

定时测试（`ScheduledTestRunnerService`）已具备探测能力，但结果与调度器完全隔离；真实请求的 `FirstTokenMs`（TTFT）和 `DurationMs`（请求时长）已写入 `usage_log`，但未被调度层感知。

## 目标

- 失败后退火加速补测，快速确认供应商是否真的不可用
- 连续失败早期新会话硬过滤，达到阈值触发 `TempUnschedulable`
- 延时和超时率感知，在新请求选号时软降权
- 粘性会话用户不受影响（除非账号真正进入 `TempUnschedulable`）

## 整体架构

三层职责分离，互不耦合：

```
┌──────────────────────────────────────────────────────────────┐
│  Layer A：退火测试状态机（内存）                                │
│  定时测试驱动，感知失败，加速补测，达阈值触发 Layer B            │
├──────────────────────────────────────────────────────────────┤
│  Layer B：TempUnschedulable（DB，现有机制）                    │
│  账号级硬不可调度，粘性会话由 shouldClearStickySession 自动清除  │
├──────────────────────────────────────────────────────────────┤
│  Layer C：调度软排序（内存 EWMA）                              │
│  TTFT + 请求时长双信号，影响新会话账号优先级                     │
└──────────────────────────────────────────────────────────────┘
```

## 缓存数据结构

```go
// AccountTestHealth 进程内内存，不持久化，不需要 Redis。
type AccountTestHealth struct {
    // --- Layer A：失败计数 ---
    ConsecFails    int           // 连续失败次数（仅定时测试维护）
    RetryInterval  time.Duration // 当前退火补测间隔
    NextRetryAt    time.Time     // 下次补测时间

    // --- Layer C：延时感知 ---
    TTFTEwma       float64       // TTFT EWMA（ms），NaN=无数据
    // 数据来源：定时测试成功 + 真实请求成功（两路共享同一 EWMA）

    SlowReqCount   int           // 滑动窗口内慢请求次数（DurationMs > SlowThreshold）
    TotalReqCount  int           // 滑动窗口内总请求次数
    WindowStart    time.Time     // 当前滑动窗口起始时间

    // --- 元信息 ---
    LastStatus     string        // "success" | "error"
    LastTestedAt   time.Time
}
```

存储：`sync.Map`，key 为 `accountID int64`，进程内单例。

进程重启后缓存为空，所有账号视为"无数据"，不惩罚，等待信号填充。

## Layer A：退火测试状态机

### 状态流转

```
正常（ConsecFails=0）
  ↓ 定时测试失败
早期失败（1 ≤ ConsecFails < HardFilterThreshold=2）
  · 新会话：正常参与调度（尚未硬过滤）
  · 粘性会话：正常服务
  · 立即触发一次补测（不等 cron，RetryInterval=0）

ConsecFails = HardFilterThreshold（=2）
  · 新会话：Layer 2 硬过滤跳过
  · 粘性会话：继续服务（绑定不清除）
  · 投退火补测（RetryInterval=30s，每次失败翻倍，上限 5min）

ConsecFails = TempUnschedThreshold（=3）
  · 触发 SetTempUnschedulable（写 DB，初始 10min）
  · 粘性会话���下次请求 shouldClearStickySession 检测 IsSchedulable()=false，清除绑定，强制重选
  · TempUnschedulable 到期后自动触发补测（现有 AutoRecover 路径）
    ├── 成功 → ConsecFails=0，RetryInterval 重置，恢复正常
    └── 失败 → 重新进入退火，TempUnschedulable 时长翻倍（上限 60min）
```

### 退火时间线（cron=5min，测试超时=10s）

```
T+0s    cron 触发，第 1 次失败（耗时 ≤10s）
          ConsecFails=1，立即投补测

T+10s   第 1 次补测，失败（耗时 ≤10s）
          ConsecFails=2，硬过滤生效，投 30s 退火补测

T+50s   第 2 次补测，失败（耗时 ≤10s）
          ConsecFails=3，触发 TempUnschedulable（10min）

从第一次失败到完全隔离：约 50 秒
```

### 关键设计：RunTestBackground 需独立超时

现有 `runOnePlan` 继承外层 5min context，供应商挂掉时测试请求可能挂住很久。退火补测必须使用独立超时：

```go
// 退火补测专用 context，独立于 cron 的 5min context
ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
result, err := s.accountTestSvc.RunTestBackground(ctx, accountID, modelID)
```

## Layer B：TempUnschedulable

直接复用现有路径，无需新增任何机制：

| 操作 | 调用 |
|------|------|
| 触发 | `accountRepo.SetTempUnschedulable(ctx, accountID, until, reason)` |
| reason | `"scheduled_test_consecutive_failures"` |
| 恢复 | `rateLimitSvc.RecoverAccountAfterSuccessfulTest`（现有 `tryRecoverAccount`） |
| 粘性清除 | `shouldClearStickySession → IsSchedulable()=false`，下次请求自动触发 |

TempUnschedulable 时长翻倍规则：

```
第 1 次触发：10min
第 2 次触发：20min
第 3 次触发：40min
上限：60min
```

## Layer C：调度软排序

### 两路信号，职责分工

| 信号 | 来源 | 用途 | 噪声 |
|------|------|------|------|
| TTFT | 定时测试成功 + 真实请求成功 | 延时排序（LatencyBucket） | 低，不受输出长度影响 |
| DurationMs | 真实请求完成 | 慢请求率统计（SlowBucket） | 高，受 prompt/输出长度影响，只做阈值判断 |

### TTFT EWMA（LatencyBucket）

两路数据写同一个 EWMA：

```
首次数据：TTFTEwma = sample（直接赋值，跳过 NaN）
后续数据：TTFTEwma = 0.3 × sample + 0.7 × TTFTEwma
```

**定时测试成功**：`sample = ScheduledTestResult.LatencyMs`
**真实请求成功**：`sample = result.FirstTokenMs`（仅流式请求有效，非流式跳过）

失败时不更新，保留最后一次成功的 EWMA 值。

分桶规则：

```
TTFTEwma = NaN（无数据）→ bucket 1（中性，不奖励不惩罚）
TTFTEwma < 1000ms       → bucket 0（快速）
1000 ≤ ms < 3000        → bucket 1（中速）
ms ≥ 3000               → bucket 2（慢速）
```

### 慢请求率（SlowBucket）

使用固定时长滑动窗口（默认 10min），统计 `DurationMs > SlowThreshold` 的请求占比：

```
窗口过期（LastTestedAt 或 LastRequestAt 距今 > 窗口长度）→ 重置计数

慢请求率 = SlowReqCount / TotalReqCount
```

分桶规则（仅在 TotalReqCount >= MinSampleCount=5 时生效，样本不足不惩罚）：

```
无数据 / 样本不足       → bucket 0（不惩罚）
慢请求率 < 20%          → bucket 0
20% ≤ 慢请求率 < 50%    → bucket 1
慢请求率 ≥ 50%          → bucket 2
```

### 完整 Layer 2 排序优先级

```
1. min(Priority)
2. min(LoadRate)
3. min(LatencyBucket)   ← TTFT EWMA 分桶
4. min(SlowBucket)      ← 慢请求率分桶
5. LRU（LastUsedAt 最久优先）
```

### 无定时测试计划的账号

```
ConsecFails 永远为 0 → 不参与退火，不触发硬过滤和 TempUnschedulable
TTFTEwma 由真实请求 TTFT 填充 → 延时排序有效
SlowBucket 由真实请求 DurationMs 填充 → 慢请求率有效
```

这类账号享受 Layer C 软排序，但没有 Layer A 的主动健康探测。

## 数据上报入口

### 入口1：定时测试完成（现有 runOnePlan）

```go
// scheduled_test_runner_service.go，runOnePlan 写 DB 后
s.healthCache.UpdateFromTest(plan.AccountID, result)

// UpdateFromTest 逻辑：
// · 成功：ConsecFails=0，RetryInterval 重置，TTFTEwma 更新
// · 失败：ConsecFails++，判断是否触发硬过滤/TempUnschedulable，投退火补测
```

### 入口2：真实请求完成（gateway_handler）

```go
// gateway_handler.go，RPM 递增之后，submitUsageRecordTask 之前
// 仅 Anthropic 平台，仅成功请求
if account.Platform == service.PlatformAnthropic && result.FirstTokenMs != nil {
    h.gatewayService.ReportAnthropicAccountTTFT(account.ID, result.FirstTokenMs)
}
if account.Platform == service.PlatformAnthropic && result.Duration > 0 {
    h.gatewayService.ReportAnthropicAccountDuration(account.ID, int(result.Duration.Milliseconds()))
}

// ReportAnthropicAccountTTFT：更新 TTFTEwma
// ReportAnthropicAccountDuration：更新 SlowReqCount / TotalReqCount
```

## 改动范围

| 文件 | 改动内容 |
|------|---------|
| 新增 `service/account_test_health_cache.go` | `AccountTestHealth` 结构体；`AccountTestHealthCache`（sync.Map 封装）；`UpdateFromTest` / `UpdateTTFT` / `UpdateDuration` / `LatencyBucket` / `SlowBucket` / `PassesHardFilter` 方法 |
| `service/scheduled_test_runner_service.go` | `runOnePlan` 更新健康缓存；退火补测延迟队列；达阈值调用 `SetTempUnschedulable`；补测使用独立 10s 超时 |
| `service/gateway_service.go` | 新增 `ReportAnthropicAccountTTFT` / `ReportAnthropicAccountDuration`；Layer 2 过滤加硬过滤判断；三级漏斗后加 LatencyBucket / SlowBucket 排序 |
| `service/wire.go` | `GatewayService` 和 `ScheduledTestRunnerService` 注入 `AccountTestHealthCache` |
| `handler/gateway_handler.go` | Anthropic 请求成功后调用两个上报方法 |
| `cmd/server/wire_gen.go` | wire 生成更新 |

不新增 DB 表，不新增 Redis key 类型，不改粘性会话路径，不改 Layer 0 / Layer 1 / Layer 1.5。

## 关键参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `HardFilterThreshold` | 2 | ConsecFails 达到此值，新会话硬过滤 |
| `TempUnschedThreshold` | 3 | ConsecFails 达到此值，触发 TempUnschedulable |
| `TempUnschedInitDuration` | 10min | TempUnschedulable 初始时长 |
| `TempUnschedMaxDuration` | 60min | TempUnschedulable 最大时长 |
| `RetryIntervalInit` | 0s | 第一次补测立即执行 |
| `RetryIntervalStep` | 30s | 之后每次失败增加的间隔 |
| `RetryIntervalMax` | 5min | 退火补测间隔上限 |
| `TestTimeout` | 10s | 退火补测专用超时 |
| `TTFTEwmaAlpha` | 0.3 | TTFT 平滑系数 |
| `LatencyBucketBounds` | 1000ms / 3000ms | 快速/中速/慢速分界 |
| `SlowThreshold` | 60s | 慢请求判定阈值 |
| `SlowWindowDuration` | 10min | 慢请求率统计窗口 |
| `SlowMinSampleCount` | 5 | 慢请求率生效最低样本数 |
| `SlowBucketBounds` | 20% / 50% | 中速/慢速分界 |


## 实施任务拆解

每个任务独立交付、独立验证、独立回滚。优先解决可用性问题，再做性能优化。

---

### P0：供应商快速隔离（可用性保障）

| # | 功能 | 交付价值 | 验收标准 |
|---|------|---------|---------|
| T1 | 定时测试失败退火加速补测 | 供应商挂掉后约 50s 感知，而不是等下一个 cron（5min） | 第一次失败后立即补测；后续间隔翻倍；补测不影响原 cron 节奏；单次补测有独立超时不阻塞 |
| T2 | 连续失败触发临时不可调度 | 不健康供应商被彻底隔离，粘性用户也切走，且到期自动恢复 | 连续失败达阈值后账号进入 TempUnschedulable；粘性用户下次请求被重新选号；到期后自动补测，成功则恢复；多次触发时隔离时长翻倍，上限 60min |

**验收**：模拟供应商连续失败，约 50s 内触发 TempUnschedulable；恢复后自动解除；粘性用户切换行为符合预期。

---

### P1：定时测试数据驱动排序（性能优化·立即可用）

| # | 功能 | 交付价值 | 验收标准 |
|---|------|---------|---------|
| T3 | 定时测试 TTFT 加入选号排序 | 有定时测试计划的账号中，响应更快的优先被选中，上线即生效 | 同优先级同负载账号中，定时测试 TTFT 低的被选中频率更高；无数据账号不被惩罚；不出现因此导致无账号可用的情况 |

**验收**：对比上线前后账号选中分布，TTFT 低的账号占比提升；无告警，无用户投诉。

---

### P2：真实请求信号补充（性能优化·覆盖更广）

| # | 功能 | 交付价值 | 验收标准 |
|---|------|---------|---------|
| T4 | 真实请求 TTFT 加入排序 | 无定时测试计划的账号也能参与延时排序，数据更贴近真实负载 | Anthropic 流式请求成功后 TTFTEwma 被更新；与定时测试数据平滑融合；非流式请求不更新 |
| T5 | 慢请求率加入选号排序 | 超时率高的供应商排序靠后，降低用户遇到慢响应的概率 | 慢请求率高的账号排序靠后；样本不足时不参与降权；不出现因此导致无账号可用的情况 |

**验收**：无定时测试计划的账号参与排序；慢账号被选中频率下降；无告警。

---

### P3：新会话硬过滤（可选增强）

| # | 功能 | 交付价值 | 验收标准 |
|---|------|---------|---------|
| T6 | 连续失败新会话硬过滤 | 在触发 TempUnschedulable 之前提前阻止新会话进入不健康账号；粘性用户不受影响 | T1/T2 稳定运行后开启；连续失败达阈值的账号不再接新会话；粘性用户流量不中断；T2 的 TempUnschedulable 仍正常兜底 |

**验收**：开启后不健康账号的新会话流量归零；粘性用户不受影响；T2 兜底逻辑仍正常工作。

---

### 优先级与依赖

```
P0  T1 退火补测
P0  T2 临时不可调度        依赖 T1
                           ↓
P1  T3 定时测试 TTFT 排序  可与 T1/T2 并行开发
                           ↓
P2  T4 真实请求 TTFT 排序  依赖 T3（共享同一缓存基础设施）
P2  T5 慢请求率排序        可与 T4 并行
                           ↓
P3  T6 新会话硬过滤        T1/T2 稳定后按需开启
```

---

## 边界情况

| 场景 | 行为 |
|------|------|
| 账号无定时测试计划 | 无 ConsecFails，Layer C 由真实 TTFT/Duration 填充，软排序有效 |
| 进程重启 | 内存缓存清空；TempUnschedulable 在 DB 中持久，仍然生效 |
| 多实例部署 | 内存缓存各实例独立；TempUnschedulable 写 DB 所有实例共享 |
| 供应商偶发抖动（1次失败后恢复） | ConsecFails=1，立即补测成功，归零，对调度无任何影响 |
| 供应商快速拒绝连接（<1s） | 退火总耗时 ~20s 完成三次失败，触发 TempUnschedulable |
| 供应商挂起不响应 | 每次测试耗尽 10s 超时，退火总耗时 ~50s 触发 TempUnschedulable |
| TempUnschedulable 期间供应商恢复 | 到期补测成功，立即恢复，ConsecFails 归零 |
| TempUnschedulable 到期仍失败 | 时长翻倍重入，最大 60min |
| TTFT 样本不足（冷启动） | NaN → bucket 1，中性对待，不影响正常账号的优先级 |
| 慢请求样本不足（< 5次） | SlowBucket=0，不惩罚 |
