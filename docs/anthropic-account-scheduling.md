# Anthropic/Claude 账号调度策略详解

> 对应代码：`backend/internal/service/gateway_service.go`

---

## 概览

Anthropic/Claude 的账号调度与 OpenAI 调度器**完全独立**，没有 EWMA 打分机制，是一套基于**多层过滤 + 优先级排序**的确定性调度体系。整个调度入口为 `SelectAccountWithLoadAwareness`（负载感知路径，主路径）和 `SelectAccountForModel`（简单路径，无并发服务时使用）。

---

## 调度入口选择

```
请求到达 gateway_handler
        │
        ▼
concurrencyService 已配置 && LoadBatchEnabled ?
   ├── 是 → SelectAccountWithLoadAwareness（负载感知，带槽位获取）
   └── 否 → SelectAccountForModel（简单路径，无并发控制）
```

**简单路径**在无并发服务时会不断重试 `SelectAccountForModel` 直到获取槽位或返回等待计划，逻辑与负载感知路径基本一致，以下重点描述负载感知路径。

---

## 前置过滤（所有路径共通）

在进入账号选择前，依次执行以下两个前置检查，任一失败直接拒绝请求：

### 1. Claude Code 客户端限制

`checkClaudeCodeRestriction` → `resolveGatewayGroup`

- 若分组配置了 `claude_code_only = true` 且当前请求**不是** Claude Code 客户端：
  - 有 `fallback_group_id`：静默切换到降级分组，继续调度
  - 无降级分组：返回 `ErrClaudeCodeOnly` 拒绝请求
- 强制平台模式（`/antigravity` 路由）跳过此检查

### 2. 渠道定价限制

`checkChannelPricingRestriction`

- 若当前分组启用了渠道定价限制且请求模型不符合渠道定价策略，直接拒绝

---

## 账号候选集加载

`listSchedulableAccounts`（优先走 `schedulerSnapshot` 缓存）

Anthropic 分组默认开启**混合调度**（`useMixed = true`），候选集包含：
- `platform = anthropic` 的原生账号
- `platform = antigravity` 且启用了 `mixed_scheduling` 的账号

加载完成后立即批量预取 **窗口费用**（`withWindowCostPrefetch`）和 **RPM 计数**（`withRPMPrefetch`），避免后续逐账号 N+1 查询。

---

## 调度分层（负载感知路径）

### Layer 0：预获取粘性会话 ID

从 Redis 缓存读取当前 `sessionHash` 对应的 `stickyAccountID`，供后续各层使用。

---

### Layer 1：模型路由（最高优先级，仅 Anthropic 分组）

`routingAccountIDsForRequest` 检查分组是否配置了 `model_routing`：

- 分组启用了 `model_routing_enabled` 且当前请求模型命中某条路由规则
- 从规则中取出指定的 `account_id` 列表作为**强制路由集合**

**路由集合内的调度流程：**

```
路由候选集过滤
  ├── isAccountSchedulableForSelection（账号整体可调度性）
  ├── isAccountAllowedForPlatform（平台匹配或混合调度许可）
  ├── isModelSupportedByAccountWithContext（模型支持）
  ├── isAccountSchedulableForModelSelection（模型级限流）
  ├── isAccountSchedulableForQuota（配额）
  ├── isAccountSchedulableForWindowCost（窗口费用，非粘性宽松）
  └── isAccountSchedulableForRPM（RPM，非粘性宽松）
        │
        ▼
Layer 1.5: 粘性会话在路由集合内？
  ├── 是 → 优先尝试粘性账号（粘性宽松检查 + 槽位获取 + 会话注册）
  └── 否 → 跳过
        │
        ▼
批量获取路由候选负载（GetAccountsLoadBatch）
        │
        ▼
过滤 LoadRate < 100 → 按 Priority → LoadRate → LastUsedAt 排序
        │
        ▼
同优先级+同负载组内随机打散（shuffleWithinSortGroups）
        │
        ▼
顺序尝试获取槽位（tryAcquireAccountSlot）+ 会话注册（checkAndRegisterSession）
  ├── 成功 → 返回，建立粘性绑定
  └── 全满 → 返回等待计划（StickySessionWaitTimeout / StickySessionMaxWaiting）

路由候选全不���用 → Fall through 到 Layer 1.5/Layer 2
```

---

### Layer 1.5：粘性会话（无模型路由时）

仅在**没有模型路由配置**时生效：

```
stickyAccountID > 0 && !excluded ?
  │
  ▼
shouldClearStickySession ?
  ├── 账号 IsSchedulable() = false → 清除 Redis 绑定，跳过
  ├── 模型级限流 GetRateLimitRemainingTime > 0 → 清除，跳过
  └── 否 → 执行粘性宽松检查
        （isAccountSchedulableForWindowCost isSticky=true
          isAccountSchedulableForRPM isSticky=true）
        │
        ▼
tryAcquireAccountSlot
  ├── 成功 → checkAndRegisterSession
  │     ├── 允许 → 刷新 TTL，返回
  │     └── 拒绝（会话数超限）→ 继续 Layer 2
  └── 失败（满载）→ 检查等待队列
        waitingCount < StickySessionMaxWaiting ?
          ├── 是 → checkAndRegisterSession → 返回等待计划
          └── 否 → 继续 Layer 2
```

**粘性宽松策略**（isSticky=true）：窗口费用和 RPM 在粘性会话下允许"黄灯"状态（`WindowCostStickyOnly`），非粘性时只允许"绿灯"（`WindowCostSchedulable`）。

---

### Layer 2：负载感知选择（兜底）

```
全量候选集过滤
  ├── !isExcluded
  ├── isAccountSchedulableForSelection
  ├── isAccountAllowedForPlatform
  ├── isModelSupportedByAccountWithContext
  ├── isAccountSchedulableForModelSelection
  ├── isAccountSchedulableForQuota
  ├── isAccountSchedulableForWindowCost (isSticky=false)
  └── isAccountSchedulableForRPM (isSticky=false)
        │
        ▼
GetAccountsLoadBatch（批量获取 Redis 并发槽位负载）
        │
        ▼
过滤 LoadRate < 100
        │
        ▼
分层过滤选择（三级漏斗）：
  1. filterByMinPriority → 只保留优先级最小（最高）的账号
  2. filterByMinLoadRate  → 只保留负载率最低的账号
  3. selectByLRU          → 选择 LastUsedAt 最久的账号
        │
        ▼
tryAcquireAccountSlot
  ├── 成功 → checkAndRegisterSession
  │     ├── 允许 → 建立粘性绑定，返回
  │     └── 拒绝 → 从 available 中移除，继续下一轮漏斗
  └── 失败（满载）→ 继续下一轮漏斗

available 耗尽：
  → 返回等待计划（FallbackWaitTimeout / FallbackMaxWaiting）
```

**GetAccountsLoadBatch 失败**时退化到 `tryAcquireByLegacyOrder`（按优先级+LRU 排序直接尝试），不中断调度。

---

## 各过滤条件详解

| 条件 | 适用账号类型 | 说明 |
|------|------------|------|
| `IsSchedulable()` | 所有 | 账号状态正常（非禁用/过载/限流标记） |
| `IsSchedulableForModelWithContext()` | 所有 | 模型级限流（`rate_limit_remaining_time > 0` 则跳过） |
| `isAccountSchedulableForQuota()` | APIKey / Bedrock | 未超出 `quota_limit` |
| `isAccountSchedulableForWindowCost()` | Anthropic OAuth / SetupToken | 当前计费窗口累计费用未超限；粘性路径宽松 |
| `isAccountSchedulableForRPM()` | Anthropic OAuth / SetupToken | 当前分钟请求数未超 `base_rpm`；粘性路径宽松 |
| `checkAndRegisterSession()` | Anthropic OAuth / SetupToken | 并发会话数未超 `max_sessions`；失败开放 |
| `isAccountInGroup()` | 粘性路径 | 账号仍属于当前分组 |
| `isUpstreamModelRestrictedByChannel()` | 有渠道限制时 | 渠道级模型访问控制 |

---

## 选择优先级规则（非路由路径）

在通过所有过滤条件的账号中，排序规则为：

```
1. Priority 数值越小 → 优先级越高（必选）
2. LoadRate 越低 → 越优先（负载感知路径）
3. LastUsedAt:
   ├── nil（从未使用）> 有使用记录（LRU）
   └── 有记录时：越早使用的越优先
```

---

## 粘性会话生命周期

| 事件 | 操作 |
|------|------|
| 账号选中（Layer 1 / 1.5 / 2��� | `SetSessionAccountID`（TTL = `stickySessionTTL`） |
| 粘性命中且正常服务 | `RefreshSessionTTL` 续期 |
| 账号 `IsSchedulable()` = false | `DeleteSessionAccountID` 清除绑定 |
| 模型级限流激活 | `DeleteSessionAccountID` 清除绑定 |
| 账号不再属于分组 | `DeleteSessionAccountID` 清除绑定 |
| 账号平台不匹配 | `DeleteSessionAccountID` 清除绑定 |

---

## 与 OpenAI 调度器的核心差异

| 维度 | Anthropic（本文） | OpenAI（`openai_account_scheduler.go`） |
|------|-------------------|----------------------------------------|
| 架构 | 内嵌 `GatewayService`，无独立调度器实例 | 独立 `defaultOpenAIAccountScheduler` |
| 选择算法 | 多层过滤 + 确定性排序 | 5 维加权打分 + top-K 加权随机 |
| TTFT | 无 | EWMA 实时统计，参与打分 |
| ErrorRate | 无打分，只影响 `IsSchedulable()` 过滤 | EWMA 实时统计，参与打分 |
| 负载感知 | LoadRate 作为排序第 2 级 | Load + Queue 共两个打分维度 |
| 模型路由 | 支持（仅 Anthropic 平台） | 不支持 |
| 会话限制 | OAuth/SetupToken 支持 max_sessions | 不涉及 |
| 窗口费用限制 | OAuth/SetupToken 支持 | 不涉及 |
| 冷启动处理 | 无（从未用账号 LRU 最优先即可） | TTFT=NaN → 退化为 0.5 中性值 |

---

## 已知缺陷与改进空间

1. **无延时感知**：调度器不感知账号的响应延时（TTFT），所有通过过滤的账号在同优先级+同负载时仅靠 LRU 区分，响应慢的账号不会被降权。

2. **ErrorRate 无差异化**：错误只有两种结果——账号被标记为不可调度（过滤掉）或正常可用，没有 EWMA 软降权机制，导致频繁出错但还未达到硬不可调度阈值的账号仍可被正常选中。

3. **冷启动无先验数据**：进程重启后不需要预热（LRU 对冷账号友好），但若希望将定时测试的延时数据引入调度，目前没有接入点。

4. **无加权随机**：同分组内确定性选择同一个最优账号，在高并发场景可能导致某个账号被连续选中直至其负载上升才轮换，而非均匀分布。
