// types.ts
// ============================================================================
// 私有扩展（不属于 upstream sub2api）：数据防泄漏（DLP）独立页面的类型定义。
//
// 本目录整体是私有新增，与 upstream 的 features/prompt-audit 并列但独立：
//   - DLP 走本地正则初筛 + LLM 二次确认，有自己的开关、检测器、确认节点与缓存；
//   - qwen3guard 内容安全走 features/prompt-audit，两者互不影响。
//
// 刻意不 import features/prompt-audit 的任何东西：
//   EventWorkspace.vue 需要反向 import 本目录的 DLP_SCANNER_CATALOG 来渲染分类
//   文案，本文件与 viewModel.ts 保持零依赖才不会形成循环引用。
//   事件相关类型只在 api.ts / DlpView.vue 里单向引用 prompt-audit。
// ============================================================================

export interface DlpEndpoint {
  id: string
  name: string
  base_url: string
  model: string
  timeout_ms: number
  enabled: boolean
  has_token: boolean
  token_status: 'configured' | 'missing' | 'invalid' | string
}

export interface DlpEndpointDraft extends DlpEndpoint {
  token: string
  clear_token: boolean
}

export interface DlpScannerDefinition {
  id: string
  label: string
  label_zh: string
  description: string
}

// 管理员可设置的严重度。刻意只有两级：low 与 medium 在拦截行为上完全一致
// （都不拦），critical 与 high 也一致，多给级别只会让人以为有行为差异。
export type DlpSeverity = 'medium' | 'high'

// DlpRule 是单条检测规则，对应后端 DLPRuleCatalogEntry。
//
// 规则表住在后端（dlpRules），整表下发而非前端硬编码：后端增删规则时界面自动
// 跟着变，不会漂移。
export interface DlpRule {
  id: string
  scanner_id: string
  title: string
  // default_severity 是代码内置值，界面用它标出「已改过默认」。
  default_severity: DlpSeverity | string
  severity: DlpSeverity | string
  disabled: boolean
  // broad 标记宽泛规则，这类误报相对高。
  broad: boolean
}

export interface DlpConfig {
  enabled: boolean
  scanners: string[]
  confirm_enabled: boolean
  confirm_timeout_ms: number
  cache_enabled: boolean
  cache_sensitive_ttl_hours: number
  cache_benign_ttl_hours: number
  block_on_high_severity: boolean
  // DLP 自己的生效范围，与 qwen3guard 的 all_groups/group_ids 完全独立。
  all_groups: boolean
  group_ids: number[]
  endpoints: DlpEndpoint[]
  available_scanners: DlpScannerDefinition[]
  // rules 是全部检测规则及其生效严重度/启停状态。
  rules: DlpRule[]
  // available_severities 是允许设置的严重度取值。
  available_severities: string[]
  // blocking_severities 是会触发拦截的严重度（前提是 block_on_high_severity 打开）。
  //
  // 为什么是这个而不是逐规则的「是否会拦」布尔值：管理员改严重度时草稿与已保存
  // 状态不一致，后端算好的布尔值当场过期，界面必须按草稿实时算。但阈值本身归后端
  // 管——它和 dlpShouldBlock 同源，前端只负责把它和草稿组合。
  blocking_severities: string[]
}

export interface DlpDraft extends Omit<DlpConfig, 'endpoints' | 'available_scanners'> {
  endpoints: DlpEndpointDraft[]
  available_scanners: DlpScannerDefinition[]
}

export interface DlpUpdateRequest {
  enabled: boolean
  scanners: string[]
  confirm_enabled: boolean
  confirm_timeout_ms: number
  cache_enabled: boolean
  cache_sensitive_ttl_hours: number
  cache_benign_ttl_hours: number
  block_on_high_severity: boolean
  all_groups: boolean
  group_ids: number[]
  endpoints: Array<{
    id: string
    name: string
    base_url: string
    model: string
    token?: string
    clear_token: boolean
    timeout_ms: number
    enabled: boolean
  }>
  // rules 提交全量列表，后端只留与内置默认值的偏差。
  rules: DlpRuleUpdate[]
}

// DlpRuleUpdate 是单条规则的写入请求。
// 用 enabled 而非 disabled，与界面上的勾选框同向。
export interface DlpRuleUpdate {
  id: string
  severity: string
  enabled: boolean
}

// ---------------------------------------------------------------------------
// Qwen3Guard 字段的原样透传
//
// DLP 与 qwen3guard 共用 PUT /admin/prompt-audit/config。该接口的
// UpdateConfigRequest 里除 expected_config_version 外都是非指针字段，省略即被
// Go 解成零值写库——DLP 页面若只提交 dlp 子树，会把 qwen3guard 的节点、分组、
// 风险分类静默清空。
//
// 因此 DLP 页面必须读全量配置、原样带回 guard 字段，只替换 dlp 子树。
// 下面两个类型就是这份「不解释、只搬运」的载荷。
// ---------------------------------------------------------------------------

// GuardPassthrough 是 GET 响应里需要原样回传的 qwen3guard 字段。
// 这里刻意用宽松的 endpoint 类型：DLP 页面从不渲染也不修改它们，
// 只做透传，字段结构由 upstream 决定，不该在本目录复述一份。
export interface GuardPassthrough {
  enabled: boolean
  blocking_enabled: boolean
  blocking_latest_turn_only: boolean
  store_pass_events: boolean
  strategy: 'priority'
  worker_count: number
  queue_capacity: number
  scanners: string[]
  all_groups: boolean
  group_ids: number[]
  endpoints: Array<Record<string, unknown>>
}

// DlpConfigResponse 是 GET /admin/prompt-audit/config 的响应中 DLP 页面关心的部分。
export interface DlpConfigResponse extends GuardPassthrough {
  config_version: number
  updated_at: string
  dlp: DlpConfig
}

// DlpPageDraft 是 DLP 页面的编辑态：dlp 子树可编辑，guard 字段只做透传。
export interface DlpPageDraft {
  config_version: number
  updated_at: string
  dlp: DlpDraft
  guard: GuardPassthrough
}

// DlpConfigUpdateRequest 是 DLP 页面提交的写入请求：guard 字段原样带回。
export interface DlpConfigUpdateRequest extends GuardPassthrough {
  expected_config_version: number
  dlp: DlpUpdateRequest
}

export interface DlpLoadErrors {
  config: string
  groups: string
  events: string
}
