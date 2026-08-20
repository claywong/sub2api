// dlpViewModel.ts
// ============================================================================
// 私有扩展（不属于 upstream sub2api）：DLP 的视图模型逻辑。
//
// 与 upstream 合并策略：
//   - 逻辑全部放本文件。upstream 的 viewModel.ts 只需 re-export 本文件并在
//     configToDraft / buildUpdateRequest 里各加一行 hook，diff 从 ~90 行降到
//     ~10 行。
// ============================================================================

import type { DlpConfig, DlpDraft, DlpEndpointDraft, DlpUpdateRequest } from './dlpTypes'

// DLP 检测器目录。
//
// 刻意与 SCANNER_CATALOG 分开维护：后者是 qwen3guard 的模型分类，会被原样发给
// 审计模型；DLP 检测器是本地正则，两者启停互不干扰。
export const DLP_SCANNER_CATALOG = [
  { id: 'dlp_credential', label: 'Credential Leak' },
  { id: 'dlp_pii', label: 'Personal Information' },
  { id: 'dlp_sensitive', label: 'Sensitive Field' },
] as const

export const DEFAULT_DLP_CONFIRM_MODEL = 'gpt-5.6-luna'
export const DEFAULT_DLP_CONFIRM_TIMEOUT_MS = 5000
export const DEFAULT_DLP_CACHE_SENSITIVE_TTL_HOURS = 6
export const DEFAULT_DLP_CACHE_BENIGN_TTL_HOURS = 24

// Vue 的 props 是 proxy，不能直接交给 structuredClone，沿用 viewModel 的 JSON 克隆。
function clone<T>(value: T): T {
  return JSON.parse(JSON.stringify(value)) as T
}

// dlpConfigToDraft 把后端配置转成草稿。
//
// config 可能缺失（后端旧版本或接口降级），一律回落到可渲染的默认值，
// 避免面板因为读不到字段而白屏。
export function dlpConfigToDraft(config?: DlpConfig | null): DlpDraft {
  return {
    enabled: config?.enabled ?? false,
    scanners: [...(config?.scanners ?? [])],
    confirm_enabled: config?.confirm_enabled ?? false,
    confirm_timeout_ms: config?.confirm_timeout_ms || DEFAULT_DLP_CONFIRM_TIMEOUT_MS,
    cache_enabled: config?.cache_enabled ?? false,
    cache_sensitive_ttl_hours:
      config?.cache_sensitive_ttl_hours || DEFAULT_DLP_CACHE_SENSITIVE_TTL_HOURS,
    cache_benign_ttl_hours: config?.cache_benign_ttl_hours || DEFAULT_DLP_CACHE_BENIGN_TTL_HOURS,
    block_on_high_severity: config?.block_on_high_severity ?? false,
    endpoints: (config?.endpoints ?? []).map((endpoint) => ({
      ...endpoint,
      token: '',
      clear_token: false,
    })),
    available_scanners: [...(config?.available_scanners ?? [])],
  }
}

export function createDefaultDlpEndpoint(index = 1): DlpEndpointDraft {
  return {
    id: `dlp-${Date.now()}-${index}`,
    name: `DLP Confirm ${index}`,
    base_url: '',
    model: DEFAULT_DLP_CONFIRM_MODEL,
    timeout_ms: DEFAULT_DLP_CONFIRM_TIMEOUT_MS,
    enabled: true,
    has_token: false,
    token_status: 'missing',
    token: '',
    clear_token: false,
  }
}

// buildDlpUpdateRequest 把草稿转成写入请求。
export function buildDlpUpdateRequest(draft: DlpDraft): DlpUpdateRequest {
  return {
    enabled: draft.enabled,
    scanners: [...draft.scanners],
    // 关闭 DLP 时同时关掉二次确认，避免后端校验"启用确认必须有可用节点"时
    // 因为一个已关闭的功能而拒绝保存。
    confirm_enabled: draft.enabled && draft.confirm_enabled,
    confirm_timeout_ms: Number(draft.confirm_timeout_ms),
    cache_enabled: draft.cache_enabled,
    cache_sensitive_ttl_hours: Number(draft.cache_sensitive_ttl_hours),
    cache_benign_ttl_hours: Number(draft.cache_benign_ttl_hours),
    block_on_high_severity: draft.block_on_high_severity,
    endpoints: draft.endpoints.map((endpoint) => ({
      id: endpoint.id.trim(),
      name: endpoint.name.trim(),
      base_url: endpoint.base_url.trim(),
      model: endpoint.model.trim() || DEFAULT_DLP_CONFIRM_MODEL,
      // 留空表示保留后端已存的 Key，因此转成 undefined 而不是空串。
      token: endpoint.token.trim() || undefined,
      clear_token: endpoint.clear_token,
      timeout_ms: Number(endpoint.timeout_ms),
      enabled: endpoint.enabled,
    })),
  }
}

export { clone as cloneDlpData }
