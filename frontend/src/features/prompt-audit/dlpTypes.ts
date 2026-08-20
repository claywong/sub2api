// dlpTypes.ts
// ============================================================================
// 私有扩展（不属于 upstream sub2api）：DLP 敏感信息检测的类型定义。
//
// 与 qwen3guard 的内容安全审计并列但独立：DLP 走本地正则初筛 + LLM 二次确认，
// 拥有自己的开关、检测器、确认节点与缓存配置。
//
// 与 upstream 合并策略：
//   - 类型全部放本文件。upstream 的 types.ts 只需 import 本文件并在
//     PromptAuditConfig / PromptAuditDraft / PromptAuditUpdateRequest 上各加
//     一个字段，diff 从 ~77 行降到 ~10 行。
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
