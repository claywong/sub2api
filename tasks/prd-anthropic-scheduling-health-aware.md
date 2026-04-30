# PRD: Anthropic 调度健康感知

## 介绍

当前 Anthropic 账号调度（Layer 2 负载感知选择）对供应商 API 账号的健康状态感知滞后——只有账号真实请求失败后才被标记为不可调度，导致在此之前延时变高或不稳定的供应商仍会持续被选中，影响用户体验。

本需求在现有调度框架（Layer 0/1/1.5/2/3）之上，新增三层健康感知机制：

- **Layer A（退火测试状态机）**：定时测试失败后加速补测，连续失败达阈值触发隔离
- **Layer B（TempUnschedulable）**：复用现有 DB 机制，实现账号级硬隔离
- **Layer C（调度软排序）**：TTFT 和慢请求率双信号，影响新会话账号优先级

目标：供应商挂掉后约 50 秒内完成感知并隔离，彻底取代"等下一个 5min cron"的被动模式。

---

## 目标

- 定时测试首次失败后立即触发补测，不等待下一个 cron 周期（5min）
- 连续失败 3 次（约 50 秒）后触发 TempUnschedulable，彻底隔离不健康账号
- 多次触发 TempUnschedulable 时隔离时长翻倍（10min → 20min → 40min → 上限 60min）
- 恢复后自动解除隔离，ConsecFails 归零
- TTFT 低的账号在同优先级同负载率时被优先选中
- 慢请求率高（>20%）的账号在新会话选号时排序靠后
- 无定时测试计划的账号仍能通过真实请求信号参与延时排序
- 粘性会话用户不受任何影响（除非账号进入 TempUnschedulable）

---

## 用户故事

### US-001: 实现 AccountTestHealthCache（内存缓存基础设施）

**描述：** 作为开发者，我需要一个进程内内存缓存来存储账号的健康状态（连续失败次数、退火参数、TTFT EWMA、慢请求统计），以便调度层可以感知供应商健康状况。

**验收标准：**
- [ ] 新增文件 `backend/internal/service/account_test_health_cache.go`
- [ ] `AccountTestHealth` 结构体包含 `ConsecFails int`、`RetryInterval time.Duration`、`NextRetryAt time.Time`、`TTFTEwma float64`（初始值 `math.NaN()`）、`SlowReqCount int`、`TotalReqCount int`、`WindowStart time.Time`、`LastStatus string`、`LastTestedAt time.Time`
- [ ] `AccountTestHealthCache` 使用 `sync.Map` 存储，key 为 `accountID int64`
- [ ] 实现 `Get(accountID int64) *AccountTestHealth` 和 `GetOrCreate(accountID int64) *AccountTestHealth` 方法
- [ ] 实现 `UpdateFromTest(accountID int64, result *ScheduledTestResult)` 方法：成功时 `ConsecFails=0` / `RetryInterval` 重置 / `TTFTEwma` 更新；失败时 `ConsecFails++`
- [ ] 实现 `UpdateTTFT(accountID int64, ttftMs int)` 方法：按 EWMA 公式更新（alpha=0.3，NaN 时直接赋值）
- [ ] 实现 `UpdateDuration(accountID int64, durationMs int)` 方法：滑动窗口统计慢请求（窗口 10min，慢请求阈值 60s）
- [ ] 实现 `LatencyBucket(accountID int64) int` 方法：NaN→1，<1000ms→0，1000-3000ms→1，≥3000ms→2
- [ ] 实现 `SlowBucket(accountID int64) int` 方法：样本<5或无数据→0；慢率<20%→0；20%-50%→1；≥50%→2
- [ ] 实现 `PassesHardFilter(accountID int64) bool` 方法：`ConsecFails >= HardFilterThreshold(=2)` 返回 false
- [ ] 所有常量定义为包级常量（`HardFilterThreshold=2`、`TempUnschedThreshold=3` 等，见文档参数表）
- [ ] 进程重启后缓存为空，不惩罚任何账号
- [ ] 编译通过，`go vet` 通过

### US-002: ScheduledTestRunnerService 集成退火补测（Layer A）

**描述：** 作为系统，我需要在定时测试失败后立即触发补测（不等待下一个 cron 周期），并在连续失败达到阈值时触发 TempUnschedulable，以便快速隔离不健康供应商。

**验收标准：**
- [ ] `ScheduledTestRunnerService` 注入 `AccountTestHealthCache` 依赖
- [ ] `runOnePlan` 执行测试后调用 `healthCache.UpdateFromTest(plan.AccountID, result)`
- [ ] `UpdateFromTest` 失败逻辑：
  - `ConsecFails=1`：立即异步投一次补测（`RetryInterval=0`）
  - `ConsecFails=2`：达到 `HardFilterThreshold`，投退火补测（`RetryInterval=30s`）
  - `ConsecFails=3`：达到 `TempUnschedThreshold`，调用 `accountRepo.SetTempUnschedulable(ctx, accountID, until, "scheduled_test_consecutive_failures")`，`until = now + TempUnschedInitDuration(=10min)`
- [ ] 每次退火失败后 `RetryInterval` 翻倍（30s → 60s → 120s → ... → 上限 5min）
- [ ] 补测使用独立 10s context：`context.WithTimeout(context.Background(), 10*time.Second)`，不继承外层 cron 的 5min context
- [ ] 补测异步执行（goroutine），不阻塞 cron 主循环
- [ ] 补测不更新 `nextRun`（不影响原 cron 节奏）
- [ ] TempUnschedulable 到期后由现有 `AutoRecover` 路径触发补测：成功则 `ConsecFails=0` / RetryInterval 重置；失败则重入退火，隔离时长翻倍（上限 60min）
- [ ] `ProvideScheduledTestRunnerService` 在 `wire.go` 中更新签名以注入 `AccountTestHealthCache`
- [ ] 编译通过，`go vet` 通过

### US-003: Layer C 延时分桶加入 Layer 2 排序

**描述：** 作为系统，我需要在 Layer 2 选号时增加 `LatencyBucket` 和 `SlowBucket` 两个排序维度，使响应更快、超时率更低的账号被优先选中。

**验收标准：**
- [ ] `filterByMinLatencyBucket(accounts []accountWithLoad, cache *AccountTestHealthCache) []accountWithLoad` 函数：过滤出 `LatencyBucket` 最小的账号集合
- [ ] `filterByMinSlowBucket(accounts []accountWithLoad, cache *AccountTestHealthCache) []accountWithLoad` 函数：过滤出 `SlowBucket` 最小的账号集合
- [ ] Layer 2 排序流水线（`gateway_service.go` 中）更新为：
  1. `filterByMinPriority`
  2. `filterByMinLoadRate`
  3. `filterByMinLatencyBucket`（仅对 Anthropic platform 生效）
  4. `filterByMinSlowBucket`（仅对 Anthropic platform 生效）
  5. `selectByLRU`
- [ ] `GatewayService` 注入 `AccountTestHealthCache` 依赖
- [ ] 无 `AccountTestHealthCache` 数据时（NaN / 样本不足），账号不被惩罚（bucket=0 或 1，与其他账号相同）
- [ ] 过滤后候选列表为空时不报错，继续兜底到下一阶段
- [ ] `ProvideGatewayService` 在 `wire.go` 中更新签名
- [ ] 编译通过，`go vet` 通过

### US-004: 真实请求 TTFT 和 DurationMs 上报（Layer C 真实信号）

**描述：** 作为系统，我需要在 Anthropic 真实请求成功后，将 FirstTokenMs 和 DurationMs 上报到 AccountTestHealthCache，使无定时测试计划的账号也能参与延时排序。

**验收标准：**
- [ ] `GatewayService` 新增 `ReportAnthropicAccountTTFT(accountID int64, firstTokenMs *int)` 方法：当 `firstTokenMs != nil` 时调用 `healthCache.UpdateTTFT`
- [ ] `GatewayService` 新增 `ReportAnthropicAccountDuration(accountID int64, durationMs int)` 方法：调用 `healthCache.UpdateDuration`
- [ ] `gateway_handler.go` 在 `submitUsageRecordTask` 之前（RPM 递增之后）添加上报调用：
  ```go
  if account.Platform == service.PlatformAnthropic && result.FirstTokenMs != nil {
      h.gatewayService.ReportAnthropicAccountTTFT(account.ID, result.FirstTokenMs)
  }
  if account.Platform == service.PlatformAnthropic && result.Duration > 0 {
      h.gatewayService.ReportAnthropicAccountDuration(account.ID, int(result.Duration.Milliseconds()))
  }
  ```
- [ ] 仅流式请求有 `FirstTokenMs`（非流式不更新 TTFTEwma）
- [ ] 仅 Anthropic 平台触发上报（非 Anthropic 跳过）
- [ ] 仅成功请求触发上报（错误请求不更新 EWMA）
- [ ] 编译通过，`go vet` 通过

### US-005: P3 新会话硬过滤（可选增强）

**描述：** 作为系统，我需要在触发 TempUnschedulable 之前，就通过硬过滤阻止新会话进入连续失败的账号，粘性用户不受影响。

**验收标准：**
- [ ] Layer 2 三级漏斗过滤阶段，在 `isAccountAllowedForPlatform` 之后新增 `PassesHardFilter` 判断
- [ ] 仅对 Anthropic 平台账号生效
- [ ] `ConsecFails >= 2` 的账号被跳过（不进入 `available` 候选列表）
- [ ] 粘性会话（Layer 1.5）不受此过滤影响（该过滤仅在 Layer 2 生效）
- [ ] T2 的 TempUnschedulable 仍作为兜底正常工作
- [ ] 编译通过，`go vet` 通过

### US-006: wire_gen.go 更新

**描述：** 作为开发者，我需要更新 `cmd/server/wire_gen.go` 以反映 `AccountTestHealthCache` 的注入。

**验收标准：**
- [ ] 运行 `wire gen ./cmd/server/` 成功，或手动同步更新 `wire_gen.go`
- [ ] `AccountTestHealthCache` 作为单例在两处注入：`NewGatewayService` 和 `NewScheduledTestRunnerService`（或通过 `ProvideXxx` 封装）
- [ ] 整个项目 `go build ./...` 通过

### US-007: 单元测试（核心逻辑）

**描述：** 作为开发者，我需要为新增的核心逻辑编写单元测试，确保退火状态机、EWMA 计算、分桶逻辑的正确性。

**验收标准：**
- [ ] 测试文件 `account_test_health_cache_test.go`（`//go:build unit`）
- [ ] 测试 `UpdateFromTest` 成功路径：`ConsecFails` 归零，`TTFTEwma` 按公式更新
- [ ] 测试 `UpdateFromTest` 失败路径：`ConsecFails` 累加，RetryInterval 翻倍，上限 5min
- [ ] 测试 `UpdateTTFT` EWMA 公式：首次直接赋值，后续 `0.3*sample + 0.7*ewma`
- [ ] 测试 `LatencyBucket` 分桶：NaN→1，<1000→0，1000-2999→1，≥3000→2
- [ ] 测试 `UpdateDuration` 窗口过期重置：超过 10min 后计数清零
- [ ] 测试 `SlowBucket` 分桶：样本<5→0；慢率<20%→0；20%-49%→1；≥50%→2
- [ ] 测试 `PassesHardFilter`：`ConsecFails=0/1`→true，`ConsecFails=2`→false
- [ ] `filterByMinLatencyBucket` / `filterByMinSlowBucket` 测试（参考现有 `scheduler_layered_filter_test.go` 风格）
- [ ] `go test -tags unit ./internal/service/...` 全部通过

---

## 功能需求

- **FR-1**: 新增 `service/account_test_health_cache.go`，封装 `AccountTestHealth` 和 `AccountTestHealthCache`，所有字段和方法见 US-001
- **FR-2**: `ScheduledTestRunnerService.runOnePlan` 在写 DB 结果后调用 `healthCache.UpdateFromTest`；失败触发退火补测（独立 10s 超时 goroutine）；达 `TempUnschedThreshold` 触发 `SetTempUnschedulable`
- **FR-3**: `GatewayService` Layer 2 在 `filterByMinLoadRate` 之后插入 `filterByMinLatencyBucket` 和 `filterByMinSlowBucket`（仅 Anthropic 平台）
- **FR-4**: `GatewayService` 新增 `ReportAnthropicAccountTTFT` 和 `ReportAnthropicAccountDuration` 方法
- **FR-5**: `gateway_handler.go` 在 Anthropic 请求成功后调用 FR-4 的两个方法
- **FR-6**: `AccountTestHealthCache` 作为单例注入 `GatewayService` 和 `ScheduledTestRunnerService`，更新 `wire.go` 和 `wire_gen.go`
- **FR-7**（可选）: Layer 2 三级漏斗增加 `PassesHardFilter` 判断（P3，T1/T2 稳定后开启）

---

## 不在范围内

- 不新增数据库表或 Redis key 类型
- 不修改 Layer 0、Layer 1、Layer 1.5、Layer 3
- 不修改粘性会话绑定路径（除 TempUnschedulable 的现有清除逻辑）
- 不对非 Anthropic 平台（OpenAI、Gemini 等）实施退火或健康感知
- 不做持久化：内存缓存进程重启后清空，不依赖 Redis
- 不做多实例同步：各实例内存独立，TempUnschedulable 写 DB 自然共享

---

## 技术约束

### 现有关键路径

| 位置 | 现状 |
|------|------|
| `service/scheduled_test_runner_service.go` | `runOnePlan` 继承外层 5min context；需新增独立 10s 超时补测路径 |
| `service/gateway_service.go` Layer 2 | 当前顺序：`filterByMinPriority` → `filterByMinLoadRate` → `selectByLRU`；需插入两个新过滤函数 |
| `internal/handler/gateway_handler.go` | `submitUsageRecordTask` 之前是 RPM 递增；新增上报调用需在 RPM 递增之后、`submitUsageRecordTask` 之前 |
| `repository/account_repo.go` | `SetTempUnschedulable(ctx, id, until, reason)` 已实现，直接复用 |
| `service/ratelimit_service.go` | `RecoverAccountAfterSuccessfulTest` 已实现（`tryRecoverAccount` 调用路径）；TempUnschedulable 到期恢复路径已有 |

### 关键参数（硬编码为常量，不需要配置化）

| 常量名 | 值 |
|------|-----|
| `HardFilterThreshold` | 2 |
| `TempUnschedThreshold` | 3 |
| `TempUnschedInitDuration` | 10min |
| `TempUnschedMaxDuration` | 60min |
| `RetryIntervalInit` | 0s（立即补测） |
| `RetryIntervalStep` | 30s |
| `RetryIntervalMax` | 5min |
| `TestTimeout` | 10s |
| `TTFTEwmaAlpha` | 0.3 |
| `LatencyBucketFast` | 1000ms |
| `LatencyBucketSlow` | 3000ms |
| `SlowThreshold` | 60s（60000ms） |
| `SlowWindowDuration` | 10min |
| `SlowMinSampleCount` | 5 |
| `SlowBucketMid` | 0.20（20%） |
| `SlowBucketHigh` | 0.50（50%） |

### TempUnschedulable 时长翻倍规则

触发时需从缓存读取上次隔离时长计算下次时长：
```
第1次: 10min
第2次: 20min
第3次: 40min
第4次+: 60min（上限）
```
实现方式：`AccountTestHealth` 新增 `TempUnschedDuration time.Duration` 字段，每次触发后翻倍并写入。

---

## 实施顺序（依赖关系）

```
US-001 账号健康缓存基础设施
  ↓
US-002 定时测试退火补测（P0）  ← 同步可开始 US-003
US-003 Layer C 延时排序（P1）
  ↓
US-004 真实请求信号上报（P2）
US-005 新会话硬过滤（P3，可选，最后开启）
  ↓
US-006 wire_gen.go 更新（与 US-001~004 联合完成后）
US-007 单元测试（与各 US 同步编写）
```

---

## 成功指标

- 模拟供应商连续 3 次失败：从第一次失败到 TempUnschedulable 生效 ≤ 60 秒
- 供应商快速恢复后：下次补测成功即解除隔离，ConsecFails 归零
- 同优先级同负载率的账号中：TTFT 低的账号被选中频率提升（可通过测试日志验证）
- 慢请求率 ≥ 50% 的账号：被选中频率明显下降
- 粘性用户：在账号未进入 TempUnschedulable 期间，流量不受任何影响

---

## 开放问题

- TempUnschedulable 时长的"翻倍计数器"是否需要单独存储（区别于 `ConsecFails`）？建议在 `AccountTestHealth` 新增 `TempUnschedDuration` 字段，进程重启后从 DB 读取现有 TempUnschedulable 状态推算，或重置为初始值（保守）。
- 退火补测是否需要向 ops 日志上报（方便排查）？建议是，复用现有 `logger.LegacyPrintf` 输出即可。
- US-005（P3 硬过滤）是否与 US-002 一起上线，还是 T1/T2 稳定后单独发布？按文档建议：T1/T2 稳定后按需开启。
