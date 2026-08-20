import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import type { DlpConfig, PromptAuditConfig } from '../types'
import DlpPanel from '../components/DlpPanel.vue'
import zhAdmin from '@/i18n/locales/zh/admin/promptAudit'
import enAdmin from '@/i18n/locales/en/admin/promptAudit'
import {
  buildUpdateRequest,
  configToDraft,
  createDefaultDlpEndpoint,
  DEFAULT_DLP_CONFIRM_MODEL,
  dlpConfigToDraft,
  DLP_SCANNER_CATALOG,
  SCANNER_CATALOG,
} from '../viewModel'

const dlpConfig = (): DlpConfig => ({
  enabled: true,
  scanners: ['dlp_pii'],
  confirm_enabled: true,
  confirm_timeout_ms: 5000,
  cache_enabled: true,
  cache_sensitive_ttl_hours: 6,
  cache_benign_ttl_hours: 24,
  block_on_high_severity: true,
  endpoints: [{
    id: 'dlp-1', name: 'Luna', base_url: 'https://api.example.com',
    model: DEFAULT_DLP_CONFIRM_MODEL, timeout_ms: 5000, enabled: true,
    has_token: true, token_status: 'configured',
  }],
  available_scanners: [],
})

const auditConfig = (dlp?: DlpConfig): PromptAuditConfig => ({
  enabled: true,
  blocking_enabled: true,
  blocking_latest_turn_only: false,
  store_pass_events: false,
  effective_mode: 'blocking',
  strategy: 'priority',
  worker_count: 4,
  queue_capacity: 100,
  scanners: SCANNER_CATALOG.map((item) => item.id),
  all_groups: true,
  group_ids: [],
  endpoints: [],
  config_version: 7,
  updated_at: '2026-07-16T00:00:00Z',
  updated_by: 1,
  change_summary: '{}',
  dlp: dlp ?? dlpConfig(),
})

// 与本目录其他测试保持一致：mock vue-i18n 让 t() 返回原始 key，
// 断言 key 而非译文——文案调整不会弄坏组件测试。
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      locale: { value: 'en' },
      t: (key: string, params?: Record<string, unknown>) =>
        key.replace(/\{(\w+)\}/g, (_, token) => String(params?.[token] ?? `{${token}}`)),
    }),
  }
})

function mountPanel(config: DlpConfig) {
  return mount(DlpPanel, { props: { draft: dlpConfigToDraft(config) } })
}

describe('DLP view model', () => {
  it('models the three DLP detectors', () => {
    expect(DLP_SCANNER_CATALOG).toHaveLength(3)
    expect(DLP_SCANNER_CATALOG.map((item) => item.id)).toEqual([
      'dlp_credential', 'dlp_pii', 'dlp_sensitive',
    ])
  })

  it('keeps DLP detectors out of the qwen3guard catalog', () => {
    // AllScannerIDs 会被原样发给审计模型，混入 DLP ID 会产出永不命中的分类。
    const guardIds = SCANNER_CATALOG.map((item) => item.id) as string[]
    for (const scanner of DLP_SCANNER_CATALOG) {
      expect(guardIds).not.toContain(scanner.id)
    }
  })

  it('falls back to renderable defaults when the backend omits dlp', () => {
    const draft = dlpConfigToDraft(undefined)
    expect(draft.enabled).toBe(false)
    expect(draft.scanners).toEqual([])
    expect(draft.endpoints).toEqual([])
    expect(draft.confirm_timeout_ms).toBeGreaterThan(0)
  })

  it('tolerates a config payload without the dlp field', () => {
    const legacy = { ...auditConfig(), dlp: undefined } as unknown as PromptAuditConfig
    const draft = configToDraft(legacy)
    expect(draft.dlp.enabled).toBe(false)
    expect(draft.dlp.endpoints).toEqual([])
  })

  it('normalizes null collections inside dlp', () => {
    const broken = {
      ...dlpConfig(), scanners: null, endpoints: null, available_scanners: null,
    } as unknown as DlpConfig
    const draft = dlpConfigToDraft(broken)
    expect(draft.scanners).toEqual([])
    expect(draft.endpoints).toEqual([])
    expect(draft.available_scanners).toEqual([])
  })

  it('strips tokens when converting the config into a draft', () => {
    const draft = dlpConfigToDraft(dlpConfig())
    expect(draft.endpoints[0].token).toBe('')
    expect(draft.endpoints[0].clear_token).toBe(false)
    // has_token 仍需保留，界面靠它提示"留空保留原 Key"
    expect(draft.endpoints[0].has_token).toBe(true)
  })

  it('includes dlp in the update request', () => {
    const request = buildUpdateRequest(configToDraft(auditConfig()))
    expect(request.dlp).toBeDefined()
    expect(request.dlp?.enabled).toBe(true)
    expect(request.dlp?.scanners).toEqual(['dlp_pii'])
    expect(request.dlp?.endpoints[0].base_url).toBe('https://api.example.com')
  })

  it('omits blank tokens so the backend keeps the stored key', () => {
    const request = buildUpdateRequest(configToDraft(auditConfig()))
    expect(request.dlp?.endpoints[0].token).toBeUndefined()
  })

  it('disables confirmation when DLP itself is off', () => {
    // 否则后端会因为"启用确认必须有可用节点"而拒绝保存一个已关闭的功能。
    const config = { ...dlpConfig(), enabled: false, confirm_enabled: true, endpoints: [] }
    const request = buildUpdateRequest(configToDraft(auditConfig(config)))
    expect(request.dlp?.confirm_enabled).toBe(false)
  })

  it('defaults the confirmation model when left blank', () => {
    const draft = configToDraft(auditConfig())
    draft.dlp.endpoints[0].model = '   '
    const request = buildUpdateRequest(draft)
    expect(request.dlp?.endpoints[0].model).toBe(DEFAULT_DLP_CONFIRM_MODEL)
  })

  it('coerces numeric fields submitted as strings', () => {
    const draft = configToDraft(auditConfig())
    draft.dlp.confirm_timeout_ms = '8000' as unknown as number
    draft.dlp.cache_benign_ttl_hours = '12' as unknown as number
    const request = buildUpdateRequest(draft)
    expect(request.dlp?.confirm_timeout_ms).toBe(8000)
    expect(request.dlp?.cache_benign_ttl_hours).toBe(12)
  })

  it('creates confirmation endpoints with the luna default', () => {
    const endpoint = createDefaultDlpEndpoint(1)
    expect(endpoint.model).toBe(DEFAULT_DLP_CONFIRM_MODEL)
    expect(endpoint.enabled).toBe(true)
    expect(endpoint.token).toBe('')
    expect(endpoint.has_token).toBe(false)
  })
})

describe('DlpPanel', () => {
  it('hides the configuration body while DLP is disabled', () => {
    const wrapper = mountPanel({ ...dlpConfig(), enabled: false })
    expect(wrapper.find('[data-test="dlp-enabled"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="dlp-confirm-enabled"]').exists()).toBe(false)
  })

  it('renders every detector plus disposition and cache controls', () => {
    const wrapper = mountPanel(dlpConfig())
    for (const scanner of DLP_SCANNER_CATALOG) {
      expect(wrapper.text()).toContain(`admin.promptAudit.dlp.detectorLabels.${scanner.id}`)
    }
    expect(wrapper.find('[data-test="dlp-block-high"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="dlp-cache-enabled"]').exists()).toBe(true)
  })

  it('treats an empty detector list as all enabled', () => {
    // 后端语义：scanners 为空表示全启用。界面必须如实反映，否则会让人误以为全关。
    const wrapper = mountPanel({ ...dlpConfig(), scanners: [] })
    const boxes = wrapper.findAll('input[type="checkbox"]')
    const checked = boxes.filter((box) => (box.element as HTMLInputElement).checked)
    expect(checked.length).toBeGreaterThanOrEqual(DLP_SCANNER_CATALOG.length)
  })

  it('warns when confirmation is enabled without an endpoint', () => {
    const wrapper = mountPanel({ ...dlpConfig(), endpoints: [] })
    expect(wrapper.text()).toContain('admin.promptAudit.dlp.endpointRequired')
  })

  it('surfaces the fail-open behaviour to the operator', () => {
    // 降级放行意味着可能漏放，界面必须明确告知而不是静默处理。
    const wrapper = mountPanel(dlpConfig())
    expect(wrapper.text()).toContain('admin.promptAudit.dlp.failOpenNotice')
  })

  it('flags endpoints whose stored key cannot be decrypted', () => {
    const config = dlpConfig()
    config.endpoints[0].token_status = 'invalid'
    const wrapper = mountPanel(config)
    expect(wrapper.text()).toContain('admin.promptAudit.dlp.tokenStatus.invalid')
  })

  it('never renders a stored token value', () => {
    const wrapper = mountPanel(dlpConfig())
    const tokenInput = wrapper.findAll('input[type="password"]')[0]
    expect((tokenInput.element as HTMLInputElement).value).toBe('')
  })

  it('emits an updated draft when toggling the master switch', async () => {
    const wrapper = mountPanel(dlpConfig())
    await wrapper.find('[data-test="dlp-enabled"]').setValue(false)
    const emitted = wrapper.emitted('update:draft')
    expect(emitted).toBeTruthy()
    expect((emitted?.[0]?.[0] as { enabled: boolean }).enabled).toBe(false)
  })

  it('adds a confirmation endpoint on demand', async () => {
    const wrapper = mountPanel({ ...dlpConfig(), endpoints: [] })
    await wrapper.find('[data-test="dlp-add-endpoint"]').trigger('click')
    const emitted = wrapper.emitted('update:draft')
    const next = emitted?.[0]?.[0] as { endpoints: unknown[] }
    expect(next.endpoints).toHaveLength(1)
  })
})

describe('DLP i18n coverage', () => {
  // 组件测试断言的是 i18n key，所以还需要单独验证 key 真的存在于两种语言里，
  // 否则漏加文案时界面会显示原始 key 而测试全绿。
  function collectKeys(node: unknown, prefix = ''): string[] {
    if (node === null || typeof node !== 'object') return [prefix]
    return Object.entries(node as Record<string, unknown>).flatMap(([key, value]) =>
      collectKeys(value, prefix ? `${prefix}.${key}` : key),
    )
  }

  const zhKeys = collectKeys(zhAdmin.promptAudit.dlp).sort()
  const enKeys = collectKeys(enAdmin.promptAudit.dlp).sort()

  it('defines the same DLP keys in Chinese and English', () => {
    expect(zhKeys).toEqual(enKeys)
  })

  it('covers every key referenced by the panel', () => {
    const required = [
      'title', 'description', 'enabled', 'detectors', 'detectorsHint',
      'disposition', 'blockOnHigh', 'blockOnHighHint',
      'confirm', 'confirmEnabled', 'confirmEnabledHint', 'confirmTimeout', 'failOpenNotice',
      'cache', 'cacheEnabled', 'cacheEnabledHint', 'cacheSensitiveTtl', 'cacheBenignTtl',
      'endpoints', 'endpointsHint', 'endpointRequired', 'addEndpoint', 'removeEndpoint',
      'endpointEnabled', 'endpointName', 'endpointBaseUrl', 'endpointModel', 'endpointTimeout',
      'endpointToken', 'clearToken', 'tokenKeepPlaceholder', 'tokenEmptyPlaceholder',
    ]
    for (const key of required) {
      expect(zhKeys).toContain(key)
    }
  })

  it('labels every detector in both languages', () => {
    for (const scanner of DLP_SCANNER_CATALOG) {
      expect(zhKeys).toContain(`detectorLabels.${scanner.id}`)
      expect(enKeys).toContain(`detectorLabels.${scanner.id}`)
    }
  })

  it('labels every token status the backend can return', () => {
    for (const status of ['configured', 'missing', 'invalid']) {
      expect(zhKeys).toContain(`tokenStatus.${status}`)
      expect(enKeys).toContain(`tokenStatus.${status}`)
    }
  })
})
