// viewModel.ts
// ============================================================================
// 私有扩展（不属于 upstream sub2api）：DLP 独立页面的视图模型。
//
// 职责：
//   - 后端配置 ↔ 页面草稿的双向转换
//   - qwen3guard 字段的原样透传（见 buildGuardPassthrough 的注释）
//   - DLP 检测器目录（供本页面与 EventWorkspace 的分类文案共用）
//
// 刻意零依赖 features/prompt-audit：EventWorkspace.vue 会反向 import 本文件的
// DLP_SCANNER_CATALOG，本文件一旦引用 prompt-audit 就会形成循环。
// ============================================================================

import type {
  DlpConfig,
  DlpConfigResponse,
  DlpConfigUpdateRequest,
  DlpDraft,
  DlpEndpointDraft,
  DlpPageDraft,
  DlpRule,
  DlpUpdateRequest,
  GuardPassthrough,
} from './types'

// DLP 检测器目录。
//
// 刻意与 qwen3guard 的 SCANNER_CATALOG 分开维护：后者是模型分类，会被原样发给
// 审计模型；DLP 检测器是本地正则，两者启停互不干扰。
export const DLP_SCANNER_CATALOG = [
  { id: 'dlp_credential', label: 'Credential Leak' },
  { id: 'dlp_pii', label: 'Personal Information' },
  { id: 'dlp_sensitive', label: 'Sensitive Field' },
] as const

// 后端未下发时的兜底值。正常情况下这两个都由 GET /config 给出，
// 这里只保证接口降级时表单仍能渲染。
export const DEFAULT_DLP_SEVERITIES = ['medium', 'high']
export const DEFAULT_DLP_BLOCKING_SEVERITIES = ['high', 'critical']

export const DEFAULT_DLP_CONFIRM_MODEL = 'gpt-5.6-luna'
export const DEFAULT_DLP_CONFIRM_TIMEOUT_MS = 5000
export const DEFAULT_DLP_CACHE_SENSITIVE_TTL_HOURS = 6
export const DEFAULT_DLP_CACHE_BENIGN_TTL_HOURS = 24

// Vue 的 props 是 proxy，不能直接交给 structuredClone。DLP 配置全是 JSON 值，
// JSON 克隆足够且不会残留响应式代理。
export function cloneDlpData<T>(value: T): T {
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
    record_regex_hits: config?.record_regex_hits ?? false,
    // 默认全部分组：与后端「未配置过 DLP 时给 all_groups=true」的表单默认一致。
    all_groups: config?.all_groups ?? true,
    group_ids: [...(config?.group_ids ?? [])],
    endpoints: (config?.endpoints ?? []).map((endpoint) => ({
      ...endpoint,
      token: '',
      clear_token: false,
    })),
    available_scanners: [...(config?.available_scanners ?? [])],
    rules: (config?.rules ?? []).map((rule) => ({ ...rule })),
    available_severities: [...(config?.available_severities ?? DEFAULT_DLP_SEVERITIES)],
    // 阈值缺失时按「high 才拦」兜底，与后端 dlpShouldBlock 的默认判定一致。
    blocking_severities: [...(config?.blocking_severities ?? DEFAULT_DLP_BLOCKING_SEVERITIES)],
  }
}

// ruleBlocks 判断一条规则在当前草稿下命中后是否会拦截请求。
//
// 三个条件都必须成立：规则未被关掉、拦截总开关已打开、严重度在后端给出的
// 拦截阈值内。阈值来自 blocking_severities 而不是硬编码 'high'，
// 后端调整阈值时界面自动跟上。
export function ruleBlocks(draft: DlpDraft, rule: DlpRule): boolean {
  if (rule.disabled || !draft.block_on_high_severity) return false
  return draft.blocking_severities.includes(rule.severity)
}

// ruleChangedFromDefault 判断规则是否被改动过（严重度改了或被关掉）。
// 界面用它标出偏离内置默认的条目，便于管理员回看自己动过什么。
export function ruleChangedFromDefault(rule: DlpRule): boolean {
  return rule.disabled || rule.severity !== rule.default_severity
}

// rulesByScanner 按检测器分组，顺序沿用后端下发的注册顺序。
export function rulesByScanner(rules: DlpRule[], scannerID: string): DlpRule[] {
  return rules.filter((rule) => rule.scanner_id === scannerID)
}

// countEnabledRules 统计某检测器下未被关掉的规则数。
// 界面用它提示「3/9 条已启用」，也用于阻止把所有规则关光后保存。
export function countEnabledRules(rules: DlpRule[], scannerID: string): number {
  return rulesByScanner(rules, scannerID).filter((rule) => !rule.disabled).length
}

// buildGuardPassthrough 抽出需要原样回传的 qwen3guard 字段。
//
// 为什么必须逐字段搬运而不是省略：
//   PUT /admin/prompt-audit/config 的 UpdateConfigRequest 里，除
//   expected_config_version 外全是非指针字段。DLP 页面若只提交 dlp 子树，
//   Go 会把缺失字段解成零值并写库——qwen3guard 的审计节点、生效分组、风险分类
//   会被静默清空，且因为 enabled=false 能通过校验，不会有任何报错提示。
//
// 每个字段都给了兜底默认值：接口降级或字段缺失时，回传的值至少能通过后端校验，
// 不会因为 undefined 被序列化丢弃而触发上面的清零路径。
export function buildGuardPassthrough(config?: Partial<GuardPassthrough> | null): GuardPassthrough {
  return {
    enabled: config?.enabled ?? false,
    blocking_enabled: config?.blocking_enabled ?? false,
    blocking_latest_turn_only: config?.blocking_latest_turn_only ?? false,
    store_pass_events: config?.store_pass_events ?? false,
    strategy: 'priority',
    worker_count: Number(config?.worker_count ?? 0),
    queue_capacity: Number(config?.queue_capacity ?? 0),
    scanners: [...(config?.scanners ?? [])],
    all_groups: config?.all_groups ?? true,
    group_ids: [...(config?.group_ids ?? [])],
    endpoints: (config?.endpoints ?? []).map((endpoint) => ({ ...endpoint })),
  }
}

// responseToPageDraft 把 GET 响应转成页面草稿。
export function responseToPageDraft(config: DlpConfigResponse): DlpPageDraft {
  return {
    config_version: config.config_version,
    updated_at: config.updated_at,
    dlp: dlpConfigToDraft(config.dlp),
    guard: buildGuardPassthrough(config),
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

// buildDlpUpdateRequest 把 DLP 草稿转成写入请求片段。
export function buildDlpUpdateRequest(draft: DlpDraft): DlpUpdateRequest {
  return {
    enabled: draft.enabled,
    scanners: [...draft.scanners],
    // 关闭 DLP 时同时关掉二次确认，避免后端校验「启用确认必须有可用节点」时
    // 因为一个已关闭的功能而拒绝保存。
    confirm_enabled: draft.enabled && draft.confirm_enabled,
    confirm_timeout_ms: Number(draft.confirm_timeout_ms),
    cache_enabled: draft.cache_enabled,
    cache_sensitive_ttl_hours: Number(draft.cache_sensitive_ttl_hours),
    cache_benign_ttl_hours: Number(draft.cache_benign_ttl_hours),
    block_on_high_severity: draft.block_on_high_severity,
    record_regex_hits: draft.record_regex_hits,
    all_groups: draft.all_groups,
    // 全部分组时清空列表，避免残留的旧选择在切回指定分组时突然生效。
    group_ids: draft.all_groups ? [] : [...draft.group_ids].sort((a, b) => a - b),
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
    // 提交全量规则列表，后端只留与内置默认值的偏差。
    rules: draft.rules.map((rule) => ({
      id: rule.id,
      severity: rule.severity,
      enabled: !rule.disabled,
    })),
  }
}

// buildConfigUpdateRequest 组装完整写入请求：guard 字段原样带回 + 新的 dlp 子树。
export function buildConfigUpdateRequest(draft: DlpPageDraft): DlpConfigUpdateRequest {
  return {
    ...buildGuardPassthrough(draft.guard),
    expected_config_version: draft.config_version,
    dlp: buildDlpUpdateRequest(draft.dlp),
  }
}

// dlpDraftFingerprint 只对 dlp 子树取指纹。
//
// 刻意不含 guard 字段：它们在本页面不可编辑，纳入指纹会让「另一个页面改了
// qwen3guard」误报成本页面有未保存改动。
export function dlpDraftFingerprint(draft: DlpPageDraft | null): string {
  if (!draft) return ''
  return JSON.stringify(buildDlpUpdateRequest(draft.dlp))
}
