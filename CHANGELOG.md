# Changelog — G7E6 AI Studio 私有 Fork

本文档记录相对于 upstream [sub2api](https://github.com/sub2api/sub2api) 的私有扩展功能。
当前 upstream 基准版本：**v0.1.179**。

> 本文档只记录本 fork 自研的功能。upstream 后续版本带来的功能（插件系统、模型广场、
> service tier 计费、WS 会话抢占、OpenAI 配额自动重置、API Key 健康熔断等）不在此列。

---

## 账号健康感知调度（Health-Aware Scheduling）

这是本 fork 最核心的差异化功能，为 Anthropic 账号构建了完整的健康感知调度体系。

### 健康三态机制
- 新增 `AccountTestHealthCache` 内存缓存基础设施，存储账号健康快照
- 账号健康状态分为三态：`Normal / StickyOnly / Excluded`，基于连续失败次数和 OTPS 阈值判定
- `HealthExcluded` 触发 `TempUnschedulable`，触发点移至状态变化回调
- 新增 `account_health.enabled` 配置开关，默认关闭，可按需启用
- 健康三态原因及滑动窗口指标通过接口暴露，前端账号列表展示 `health_verdict`
- 账号列表"恢复状态"按钮支持健康降级态，同时清除健康缓存解除 Excluded 死锁
- 阈值可配置：`StickyOnly=3 次, Excluded=5 次, OTPS<20`（运行时读取配置而非硬编码默认值）

### 调度排序与分桶
- TTFT/OTPS 质量分桶调度：按响应速度对账号分级排序
- Layer 2 硬过滤：跳过连续失败的 Anthropic 账号（新会话）
- Layer C 延时分桶：真实请求 TTFT 和 DurationMs 上报到健康缓存
- 按 `account+model` 维度分桶调度，新增缓存命中率优先级
- 新增 TCP 连接时间（`tcp_conn_avg_ms`）和 TTFB（`ttfb_avg_ms`）网络层细粒度指标
- 健康缓存新增缓存命中率统计指标

### 定时测试集成
- `ScheduledTestRunnerService` 集成退火补测（Layer A）
- 定时测试失败后增加指数退避补测逻辑
- `ConsecFails` 仅由定时测试驱动，真实请求不再影响（移除退火机制，仅保留 HealthVerdict 滑动窗口）
- 新增 `scheduled_test_results.first_token_ms` 字段迁移
- 修复调度漂移和跳过时不推进 `next_run_at` 的问题

### 上游错误阈值调度
- 上游错误（500/502/520）触发临时不可调度
- pool 模式重试期间同时执行上游错误阈值计数
- 同账号重试次数读取账号配置而非硬编码
- pool 模式 side effects 延迟至同账号重试耗尽后执行
- 可配置 pool 模式同账号重试触发的状态码列表

---

## 数据防泄漏（Prompt DLP）

体量上与健康调度并列的第二大自研子系统，独立于 upstream 的提示词审计。

- 检测链路：**正则初筛 → 排除链 → 算法校验 → 检测器间去重 → LLM 二次确认 → 分级拦截**。
  只有通过初筛存活的 finding 才会走 LLM 确认，以此压低请求量和成本
- 检测入口设在 `Coordinator` 层，修复了挂在审计模式下会随审计开关静默失效的问题
- DLP 拆成独立页面（`/admin/dlp`），事件按检测来源分流，带 `requiresRiskControl` 权限门
- 检测规则的严重度与启停可配置，支持规则覆写；补了前后端配置契约测试，堵住规则表下发的验证盲区
- 扫描范围收窄到用户输入与工具输出（tool result 快照），证据对管理员不脱敏
- 新增「命中但放行也记为事件」开关，便于灰度期观察误报
- 顶层开关移到底部保存栏，与内容安全页面保持一致

---

## Layer 2 性价比选号（weighted_selection）

默认关闭，可配置启用。CHANGELOG 早期版本称其"已移除"，实为下线后重新引入并化简。

- 打分公式化简为一行性价比：`score = quality / effRate^β`，
  其中 `quality = 0.6 × ttftScore + 0.4 × otpsScore`
- 缓存命中率作为带内成本因子参与加权
- v2 迭代：确定性选号 + 容差带 + 冷启动乐观估计，消除同分抖动
- 新增 `GET /api/v1/admin/scheduler/quality` 观测接口（含 `score` 字段），
  以及 admin 侧调度器只读 dump（weighted metrics + 健康快照）
- 配套 `AccountModelQualityCache` 及单测，覆盖渠道映射 key 一致性；
  修复 quality cache 写入 key 未用原始 `reqModel` 导致渠道映射后分桶失效

---

## 指标冷却（Metric Cooldown）

- 周期扫描 `usage_logs` 指标，命中阈值即打 `manual-cooldown`
- 引入 `manual-cooldown` 前缀，定时测试的 auto-recover 不再误清除人工冷却
- 覆盖面板简化为单开关 + 一行 4 个阈值
- `metric_cooldown_override` 处理移至 platform-specific 块之后，避免被平台逻辑覆盖

---

## 飞书离职自动禁用

- 定时任务比对飞书在职状态，自动禁用离职人员账号；管理页提供配置入口、
  连通性测试、手动触发与执行历史（`906` 迁移）
- 修正离职判定：多条邮箱匹配记录时任一在职即不禁用
- 拦掉面板内回车，避免误提交外层系统设置表单

---

## 分组用量监控

- `/monitor` 页新增分组消耗区块，全链路自研（repo / service / handler / 独立路由文件）
- 分组消耗按用户完整可见分组口径过滤，修正越权可见问题

---

## 会话与粘性控制

- 会话数量控制放开到 Anthropic API Key 账号（upstream 仅支持 OAuth / SetupToken），
  抽出 `SupportsSessionLimit` 谓词替换原 `IsAnthropicOAuthOrSetupToken` 判定
- 粘性会话豁免并发上限
- 账号级「救火号」开关：failover 重试命中时不接管粘性会话

---

## 管理员访问控制

- 管理员 API Key 增加 IP 白名单访问控制

---

## 订阅计费

- 订阅分组支持额度耗尽后回退到余额计费

> 会话级模型锁定与受保护模型额度管理已下线。`groups` 表的 `protected_models`
> 与 `protected_model_quotas` 两列暂作死列保留，以留出回滚余地；清理方式见
> `backend/migrations/README.md`。

---

## 请求日志（Request Logs）

- 新增 `request_logs` 表及请求日志记录服务，完善 `usage` 统计 `request_type` 支持
- 全协议捕获并精简 request/response 内容
- 补全 `chat_completions` 路径的 request_log 写入
- 统一 `session_id` 来源，加长度截断与 JSON 安全裁剪
- 支持 `max_body_bytes` 配置（未配置时不做隐式 4KB 截断）

---

## Anthropic 账号专属配置

- 支持 Anthropic API Key 账号单独配置缓存 TTL Override
- 新增 Anthropic 上游专属 `anthropic_response_header_timeout` 配置（Transport 层实现，仅对流式请求生效，非流式使用通用 `response_header_timeout`）
- 新增 **Anthropic full passthrough 模式**（完整透传，不做协议转换）
- 修复 `anthropic_response_header_timeout` 对 passthrough 账号完全无效的问题
- 修复 `按最终 anthropic-beta header` 对 `body.context_management` 做能力维度 sanitize

---

## OpenAI 网关扩展

- 新增 **OpenAI embeddings gateway**，支持 `/v1/embeddings` 接口路由
- `codex_cli_only` 新增放行 Claude Code Codex 插件的机制
- 流超时触发阈值上限从 10 次放宽到 60 次
- 修复 WebSocket 超大请求桥接
- 修复 OpenAI WS 限速时未触发账号 failover 的问题
- 注入 WS Codex 生图桥接工具
- 修复 failover 时残留缓存请求体导致重映射错误的问题
- 网络层超时统一触发 failover，池模式同时支持同账号重试
- 数据间隔超时改按「有效 data 行」计时，防中转空心跳无限续命（原生 Anthropic 直通路径同步）
- 流式响应中途静默断连时补发 error 事件通知客户端
- OAuth 账号 `count_tokens` 不支持时避免误触发账号冷却

---

## Quota 与计费优化

- `user×platform` 配额 DB 写聚合 flusher（减少高频写入）
- sentinel 回填消除无配额行用户 preflight 每请求回源 DB
- 支持按 5h/7d 用量阈值自动暂停账号调度
- 修正 `allow_balance_fallback` 永远为 false 的问题（`GetByKeyForAuth` 补全 Group SELECT 字段）
- 修复长上下文场景 `cache_creation` 和 `cache_read` 未应用长上下文倍率的计费问题
- 余额兜底首请求即生效，避免先报错再重试
- 窗口重置期间跳过 Redis 二次校验，避免误触余额兜底

---

## 用量统计增强

- `user-breakdown` 默认 limit 从 50 改为 300，上限放开至 300
- `/admin/usage` 支持查看已删除用户的历史使用情况
- 账号用量窗口 5h/7d 增加说明 tooltip
- 优化 `/admin/usage` 打开速度与刷新响应
- 修正 OpenAI 5h 用量百分比语义（直接使用 5h 窗口数据）
- 用量明细展示 OTPS，统一扣除首字延迟口径（`usageOtps.ts`）

---

## 分组功能

- 支持自定义 `/v1/models` 模型列表（按分组配置可见模型）
- GLM 分组出站 Anthropic 指纹归一化开关（`907` 迁移）

---

## 管理后台与前端

- 账号列表支持按模型名称筛选（下拉选择器，从 `model_mapping` 动态加载）
- 移除账号列表的类型筛选条件
- 账号管理列表新增创建时间列
- 优化账号列表显示
- 在失败日志中补充 `account_name` 字段，便于排查
- 编辑账号弹窗修复：apikey 类型下缓存 TTL 设置不显示的问题
- 编辑账号选择兼容模式时避免 quota 块回退旧 extra 字段
- 账号用量窗口 5h/7d 增加说明 tooltip
- 简化首页和登录页，关闭注册等公开路由
- 禁止用户编辑用户名和更换邮箱
- 替换项目 logo 为新版 G7E6 AI Studio 图标
- 关闭的公开路由：`/register`、`/forgot-password`、`/reset-password`、`/email-verify`、
  `/key-usage`、`/legal`、`/setup`，访问直接重定向到 `/login`
- 简化 auth-layout logo 块，移除未定义的 `settingsLoaded` / `siteSubtitle`
- 转义邮箱占位符里的 `@`，修复添加通知邮箱时白屏
- 补齐 zh / en 缺失的界面文案，新增 i18n 文案编译校验测试
- 钉住前端 pnpm 版本为 9.15.9

---

## 公开 API 扩展

- 新增 `GET /api/v1/key-rate` 接口，查询当前 key 的费率倍率

---

## 运维与监控

- 新增 Ops 错误 Webhook dispatcher，支持实时错误推送到外部系统
- 在失败日志中补充 `account_name` 字段
- 增强渠道监控检查器的调试信息
- 修复 antigravity Gemini 限速和账号调度问题
- 修复并发获取失败的错误分类
- 新增 HTTP trace 采集（`http_trace.go`），支撑 TCP / TTFB 网络层细粒度指标
- 新增独立的上游错误计数缓存（`error_counter_cache.go`）与 Lua 计数脚本
- 定时测试仅跳过手动关闭调度的账号，并为 errored 账号保留 schedulable 标记
- 补测成功后触发 auto-recover 恢复账号状态
- 记录 `claude_code_only` 分组拒绝请求的失败原因
- Claude Code 校验器增加诊断信息（`claude_code_validator_diag.go`）
- 上游健康失败统计与 `error_owner` 分类对齐
- 补全调度快照缓存中缺失的配额和限流字段

---

## 模型支持

- 适配 `claude-opus-4-8`
- 模型定价元数据更新

---

## 架构重构（维护性）

- 拆分 4 个核心文件到 companion 文件，迁移号段迁到 9XX 私有段
- 加权调度算法一度移除后重新引入并化简（详见「Layer 2 性价比选号」章节），当前默认关闭
- `Codex Responses ↔ Chat Completions` 桥接重设计
- gateway 层多项重构：延迟 OpenAI 请求 map 解码、引入 request body refs、隔离 anthropic body rewrites 等
- 修复 pool 模式 OAuth 401 handler 误覆盖 credentials 的问题
