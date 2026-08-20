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
